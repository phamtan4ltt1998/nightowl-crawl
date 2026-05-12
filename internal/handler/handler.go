// Package handler provides HTTP handlers for the NightOwl fetcher service.
// All endpoints stream results as NDJSON (one JSON object per line).
package handler

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/nightowl/fetcher/internal/crawler"
	"github.com/nightowl/fetcher/internal/parse"
)

// Handler holds the HTTP handlers.
type Handler struct {
	parser  *parse.Parser
	crawler *crawler.Crawler
}

// New creates a Handler.
func New(parser *parse.Parser, c *crawler.Crawler) *Handler {
	return &Handler{parser: parser, crawler: c}
}

// Health handles GET /health.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// FetchListingRequest is the body for POST /fetch/listing.
type FetchListingRequest struct {
	URL string `json:"url"`
}

// FetchListing handles POST /fetch/listing.
// Streams story refs as NDJSON: {"type":"story_ref","url":"..."}
func (h *Handler) FetchListing(w http.ResponseWriter, r *http.Request) {
	var req FetchListingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if req.URL == "" {
		http.Error(w, "url required", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	enc := json.NewEncoder(w)
	ch := make(chan string, 100)

	go func() {
		defer close(ch)
		if err := h.parser.FetchListing(r.Context(), req.URL, ch); err != nil {
			log.Error().Err(err).Str("url", req.URL).Msg("listing crawl error")
		}
	}()

	count := 0
	for storyURL := range ch {
		enc.Encode(map[string]string{"type": "story_ref", "url": storyURL})
		flusher.Flush()
		count++
	}
	log.Info().Str("url", req.URL).Int("stories", count).Msg("listing done")
}

// FetchStoryRequest is the body for POST /fetch/story.
type FetchStoryRequest struct {
	URL      string `json:"url"`
	RenderJS bool   `json:"render_js"`
}

// FetchStory handles POST /fetch/story.
// Streams NDJSON:
//   - {"type":"story_meta", "data":{...}} — first line
//   - {"type":"chapter",    "data":{...}} — one per chapter (out-of-order)
//   - {"type":"done",       "count":N}    — last line
func (h *Handler) FetchStory(w http.ResponseWriter, r *http.Request) {
	var req FetchStoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if req.URL == "" {
		http.Error(w, "url required", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()
	var streamMu sync.Mutex
	enc := json.NewEncoder(w)

	streamLine := func(v any) {
		streamMu.Lock()
		enc.Encode(v)
		flusher.Flush()
		streamMu.Unlock()
	}

	// 1. Fetch story metadata
	meta, err := h.parser.FetchStoryMeta(ctx, req.URL)
	if err != nil {
		log.Warn().Err(err).Str("url", req.URL).Msg("meta fetch failed, using empty")
		meta = &parse.StoryMeta{URL: req.URL}
	}
	streamLine(map[string]any{"type": "story_meta", "data": meta})

	// 2. Collect all chapter refs
	refCh := make(chan parse.ChapterRef, 200)
	go func() {
		defer close(refCh)
		if err := h.parser.FetchChapterList(ctx, req.URL, refCh); err != nil {
			log.Error().Err(err).Msg("chapter list error")
		}
	}()

	var refs []parse.ChapterRef
	for ref := range refCh {
		refs = append(refs, ref)
	}
	log.Info().Str("url", req.URL).Int("chapters", len(refs)).Msg("chapter list collected")

	// 3. Fetch chapter content concurrently (max 4 workers)
	const workers = 4
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for _, ref := range refs {
		ref := ref
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			// Small jitter: 300–1300ms to avoid synchronized bursts
			time.Sleep(time.Duration(300+rand.Intn(1000)) * time.Millisecond)

			ch, err := h.parser.FetchChapter(ctx, ref)
			if err != nil {
				log.Warn().Err(err).Int("chapter", ref.Number).Msg("chapter fetch failed")
				return
			}
			streamLine(map[string]any{"type": "chapter", "data": ch})
		}()
	}

	wg.Wait()
	streamLine(map[string]any{"type": "done", "count": len(refs)})
	log.Info().Str("url", req.URL).Int("chapters", len(refs)).Msg("story fetch done")
}

// CrawlStoriesRequest is the body for POST /crawl/stories.
type CrawlStoriesRequest struct {
	URLs        []string `json:"urls"`
	Concurrency int      `json:"concurrency"` // per-story workers, default 2
}

// CrawlStories handles POST /crawl/stories.
// Triggers full crawl (disk + DB) for each URL concurrently and streams NDJSON results:
//
//	{"url":"...","slug":"...","new_chapters":5,"total_chapters":120}
//	{"url":"...","slug":"...","error":"..."}
//	{"type":"done","total":3,"failed":0}
func (h *Handler) CrawlStories(w http.ResponseWriter, r *http.Request) {
	if h.crawler == nil {
		http.Error(w, "crawler not available", http.StatusServiceUnavailable)
		return
	}

	var req CrawlStoriesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if len(req.URLs) == 0 {
		http.Error(w, "urls required", http.StatusBadRequest)
		return
	}
	if len(req.URLs) > 50 {
		http.Error(w, "max 50 urls per request", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	enc := json.NewEncoder(w)
	var mu sync.Mutex
	streamLine := func(v any) {
		mu.Lock()
		enc.Encode(v)
		flusher.Flush()
		mu.Unlock()
	}

	out := make(chan crawler.StoryResult, len(req.URLs))

	go func() {
		h.crawler.CrawlURLs(r.Context(), req.URLs, req.Concurrency, out)
		close(out)
	}()

	failed := 0
	for res := range out {
		if res.Error != "" {
			failed++
		}
		streamLine(res)
	}

	streamLine(map[string]any{
		"type":   "done",
		"total":  len(req.URLs),
		"failed": failed,
	})
	log.Info().Int("total", len(req.URLs)).Int("failed", failed).Msg("batch crawl done")
}
