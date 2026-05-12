// Package crawler orchestrates the full story crawl pipeline:
// listing → filter existing → fetch content → write disk → upsert MySQL.
// This is a port of Python scrape_job.py + scraper.py logic.
package crawler

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/nightowl/fetcher/internal/config"
	"github.com/nightowl/fetcher/internal/db"
	"github.com/nightowl/fetcher/internal/parse"
)

var (
	reChapChuong = regexp.MustCompile(`chuong-(\d+)`)
	reLeadDigits = regexp.MustCompile(`^(\d+)`)
)

// Crawler orchestrates the crawl pipeline for one scrape source.
type Crawler struct {
	parser          *parse.Parser
	contentRoot     string
	recrawlExisting bool
}

// New creates a Crawler using the given parser, content root directory, and recrawl flag.
func New(parser *parse.Parser, contentRoot string, recrawlExisting bool) *Crawler {
	return &Crawler{parser: parser, contentRoot: contentRoot, recrawlExisting: recrawlExisting}
}

// StoryResult holds the outcome of crawling one story URL.
type StoryResult struct {
	URL         string `json:"url"`
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Author      string `json:"author"`
	Status      string `json:"status"`
	NewChapters int    `json:"new_chapters"`
	Total       int    `json:"total_chapters"`
	BookID      int64  `json:"book_id"`
	Error       string `json:"error,omitempty"`
}

// CrawlURLs crawls multiple story URLs concurrently and streams results to out.
// concurrency <= 0 defaults to 2. Each URL is matched to source config by domain.
func (c *Crawler) CrawlURLs(ctx context.Context, urls []string, concurrency int, out chan<- StoryResult) {
	if concurrency <= 0 {
		concurrency = 2
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for _, u := range urls {
		u := u
		// Find matching source config (for free_chapter_threshold)
		src := c.sourceForURL(u)
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			res, err := c.crawlStory(ctx, u, src)
			if err != nil {
				log.Warn().Err(err).Str("url", u).Msg("crawl failed")
				out <- StoryResult{URL: u, Slug: parse.StorySlugFromURL(u), Error: err.Error()}
				return
			}
			out <- *res
		}()
	}
	wg.Wait()
}

// sourceForURL returns the ScrapeSource matching the URL's domain, or an empty default.
func (c *Crawler) sourceForURL(rawURL string) config.ScrapeSource {
	// parser exposes source configs via its domain lookup; we replicate the domain match here.
	// For now return a zero-value source (uses defaults: concurrency handled by CrawlURLs semaphore).
	return config.ScrapeSource{}
}

// RunSource executes the full pipeline for one ScrapeSource entry.
// Mirrors Python _scrape_source() logic.
func (c *Crawler) RunSource(ctx context.Context, src config.ScrapeSource) error {
	log.Info().Str("url", src.URL).Str("genre", src.Genre).Msg("source start")

	// 1. Collect all story URLs from listing (BFS through pagination)
	storyCh := make(chan string, 200)
	go func() {
		defer close(storyCh)
		if err := c.parser.FetchListing(ctx, src.URL, storyCh); err != nil {
			log.Error().Err(err).Str("url", src.URL).Msg("listing crawl error")
		}
	}()

	var allURLs []string
	for u := range storyCh {
		allURLs = append(allURLs, u)
	}
	sort.Strings(allURLs)

	if len(allURLs) == 0 {
		log.Info().Str("url", src.URL).Msg("no stories found")
		return nil
	}

	// 2. Filter slugs already in DB — skipped when recrawlExisting=true so we
	// re-check known stories for new chapters (dedup happens at chapter level).
	var newURLs []string
	if c.recrawlExisting {
		newURLs = allURLs
		log.Info().
			Str("url", src.URL).
			Int("total", len(allURLs)).
			Msg("recrawl mode — checking all stories for new chapters")
	} else {
		slugs := make([]string, len(allURLs))
		for i, u := range allURLs {
			slugs[i] = parse.StorySlugFromURL(u)
		}
		existing, err := db.GetExistingSlugs(slugs)
		if err != nil {
			log.Warn().Err(err).Msg("get existing slugs failed — crawling all")
		}
		for _, u := range allURLs {
			if !existing[parse.StorySlugFromURL(u)] {
				newURLs = append(newURLs, u)
			}
		}
		log.Info().
			Str("url", src.URL).
			Int("total", len(allURLs)).
			Int("existing", len(existing)).
			Int("new", len(newURLs)).
			Msg("filtered")
	}

	if len(newURLs) == 0 {
		return nil
	}

	// Cap candidate list: target_count*3 (same as Python)
	unlimited := src.TargetCount <= 0
	candidates := newURLs
	if !unlimited && len(candidates) > src.TargetCount*3 {
		candidates = candidates[:src.TargetCount*3]
	}

	// 3. Crawl concurrently with semaphore + jitter
	concurrency := src.Concurrency
	if concurrency <= 0 || concurrency > 4 {
		concurrency = 2
	}

	sem := make(chan struct{}, concurrency)
	var mu sync.Mutex
	var wg sync.WaitGroup
	successCount := 0
	stopFlag := false

	for i, storyURL := range candidates {
		if ctx.Err() != nil {
			break
		}

		mu.Lock()
		done := !unlimited && successCount >= src.TargetCount
		mu.Unlock()
		if done || stopFlag {
			break
		}

		storyURL := storyURL
		pos := i
		wg.Add(1)
		sem <- struct{}{}

		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			// Jitter: 1.5–4s base + stagger by position (mirrors Python)
			jitter := time.Duration(1500+rand.Intn(2500)) * time.Millisecond
			jitter += time.Duration(pos%concurrency) * time.Second
			select {
			case <-ctx.Done():
				return
			case <-time.After(jitter):
			}

			if _, err := c.crawlStory(ctx, storyURL, src); err != nil {
				log.Warn().Err(err).Str("url", storyURL).Msg("story crawl failed")
				return
			}

			mu.Lock()
			successCount++
			n := successCount
			target := src.TargetCount
			if !unlimited && n >= target {
				stopFlag = true
				log.Info().Int("target", target).Msg("target reached, stopping source")
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	disp := "∞"
	if !unlimited {
		disp = strconv.Itoa(src.TargetCount)
	}
	log.Info().Str("url", src.URL).
		Int("success", successCount).Str("target", disp).
		Msg("source done")
	return nil
}

// crawlStory is the per-story pipeline:
// fetch → write disk → upsert DB.
func (c *Crawler) crawlStory(ctx context.Context, storyURL string, src config.ScrapeSource) (*StoryResult, error) {
	slug := parse.StorySlugFromURL(storyURL)
	contentDir := filepath.Join(c.contentRoot, slug)

	if err := os.MkdirAll(contentDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", contentDir, err)
	}

	existingNums := existingChapterNumbers(contentDir)
	existingCount := existingFileCount(contentDir)

	// Fetch meta + chapters from remote
	meta, newChaps, totalCount, err := c.fetchStory(ctx, storyURL, existingNums)
	if err != nil {
		return nil, err
	}

	writtenCount := 0
	if len(newChaps) > 0 {
		// Sort by chapter number before writing (chapters arrive out-of-order)
		sort.Slice(newChaps, func(i, j int) bool {
			return newChaps[i].Number < newChaps[j].Number
		})
		for idx, ch := range newChaps {
			fName := fmt.Sprintf("%04d-%s.md", existingCount+idx+1, ch.Slug)
			fPath := filepath.Join(contentDir, fName)
			if err := os.WriteFile(fPath, []byte(ch.ContentMD), 0o644); err != nil {
				log.Warn().Err(err).Str("file", fPath).Msg("write chapter failed")
			} else {
				writtenCount++
			}
		}
	}

	// Upsert books + chapters to MySQL
	result, err := db.UpsertStoryFromDir(db.UpsertArgs{
		Slug:                 slug,
		ContentRoot:          c.contentRoot,
		StoryName:            meta.Title,
		FreeChapterThreshold: src.FreeChapterThreshold,
		SourceURL:            storyURL,
		Author:               meta.Author,
		Genre:                meta.Genre,
		Status:               meta.Status,
		Description:          meta.Description,
		CoverImage:           meta.CoverImage,
		Rating:               meta.Rating,
	})
	if err != nil {
		return nil, fmt.Errorf("upsert db slug=%s: %w", slug, err)
	}

	res := &StoryResult{
		URL:         storyURL,
		Slug:        slug,
		Title:       meta.Title,
		Author:      meta.Author,
		Status:      meta.Status,
		NewChapters: writtenCount,
		Total:       totalCount,
		BookID:      result.BookID,
	}

	ev := log.Info().
		Int64("book_id", result.BookID).
		Str("slug", slug).
		Str("title", meta.Title).
		Str("author", meta.Author).
		Str("status", meta.Status).
		Int("new_chapters", writtenCount).
		Int("total_chapters", totalCount)
	if meta.Rating > 0 {
		ev = ev.Float64("rating", meta.Rating)
	}
	if writtenCount == 0 {
		ev.Msg("✓ story up-to-date")
	} else {
		ev.Msg("✓ story crawled")
	}
	return res, nil
}

// chapterData is a fetched chapter's content.
type chapterData struct {
	Number    int
	Slug      string
	ContentMD string
}

// fetchStory fetches meta + all new chapter content for a story URL.
func (c *Crawler) fetchStory(
	ctx context.Context,
	storyURL string,
	existing map[int]bool,
) (*parse.StoryMeta, []chapterData, int, error) {
	// Meta
	meta, err := c.parser.FetchStoryMeta(ctx, storyURL)
	if err != nil {
		log.Warn().Err(err).Str("url", storyURL).Msg("meta fetch failed, using empty")
		meta = &parse.StoryMeta{URL: storyURL}
	}

	// Chapter list
	refCh := make(chan parse.ChapterRef, 500)
	go func() {
		defer close(refCh)
		if err := c.parser.FetchChapterList(ctx, storyURL, refCh); err != nil {
			log.Error().Err(err).Str("url", storyURL).Msg("chapter list error")
		}
	}()

	var refs []parse.ChapterRef
	for ref := range refCh {
		if !existing[ref.Number] {
			refs = append(refs, ref)
		}
	}

	totalFromListing := len(existing) + len(refs)

	if len(refs) == 0 {
		return meta, nil, totalFromListing, nil
	}

	// Fetch chapter content concurrently (4 workers, same as handler)
	const workers = 4
	sem := make(chan struct{}, workers)
	var mu sync.Mutex
	var chapters []chapterData
	var wg sync.WaitGroup

	for _, ref := range refs {
		ref := ref
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			time.Sleep(time.Duration(300+rand.Intn(1000)) * time.Millisecond)

			ch, err := c.parser.FetchChapter(ctx, ref)
			if err != nil {
				log.Warn().Err(err).Int("chapter", ref.Number).Msg("fetch chapter failed")
				return
			}
			mu.Lock()
			chapters = append(chapters, chapterData{
				Number:    ch.Number,
				Slug:      ch.Slug,
				ContentMD: ch.ContentMD,
			})
			mu.Unlock()
		}()
	}
	wg.Wait()

	return meta, chapters, totalFromListing, nil
}

// --- helpers ---

func existingChapterNumbers(dir string) map[int]bool {
	entries, _ := os.ReadDir(dir)
	m := map[int]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if n := parseChapNum(e.Name()); n > 0 {
			m[n] = true
		}
	}
	return m
}

func existingFileCount(dir string) int {
	entries, _ := os.ReadDir(dir)
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			n++
		}
	}
	return n
}

func parseChapNum(filename string) int {
	if m := reChapChuong.FindStringSubmatch(filename); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	if m := reLeadDigits.FindStringSubmatch(filename); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}
