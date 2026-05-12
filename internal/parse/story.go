package parse

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/nightowl/fetcher/internal/config"
)

// FetchStoryMeta fetches and parses metadata from a story's main page.
func (p *Parser) FetchStoryMeta(ctx context.Context, storyURL string) (*StoryMeta, error) {
	body, err := p.client.Get(ctx, storyURL)
	if err != nil {
		return nil, fmt.Errorf("fetch story meta: %w", err)
	}
	src := p.sourceConfigFor(storyURL)
	return parseStoryMeta(body, storyURL, src)
}

// FetchChapterList crawls a story page (and its chapter-list pagination) and sends
// each discovered ChapterRef to ch in ascending chapter-number order.
func (p *Parser) FetchChapterList(ctx context.Context, storyURL string, ch chan<- ChapterRef) error {
	storyKey := storyKeyFromURL(storyURL)
	allowedDomain := domainOf(storyURL)
	chapterRe := regexp.MustCompile(
		`(?i)/` + regexp.QuoteMeta(storyKey) + `/chuong-(\d+)(?:\.html|/?)\z`,
	)

	visited := make(map[string]bool)
	queue := []string{normalizeURL(storyURL)}
	collected := make(map[string]ChapterRef)

	for len(queue) > 0 {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		pageURL := queue[0]
		queue = queue[1:]

		if visited[pageURL] {
			continue
		}
		visited[pageURL] = true

		body, err := p.client.Get(ctx, pageURL)
		if err != nil {
			continue
		}

		chapters, pages := extractChapterLinks(body, storyURL, chapterRe, allowedDomain)
		for _, c := range chapters {
			collected[c.URL] = c
		}
		for _, pg := range pages {
			if !visited[pg] {
				queue = append(queue, pg)
			}
		}
	}

	for _, ref := range sortChapterRefs(collected) {
		ch <- ref
	}
	return nil
}

var reDescPrefix = regexp.MustCompile(`(?i)^Giới Thiệu\s*:\s*`)
var reRatingNum = regexp.MustCompile(`(\d+(?:[.,]\d+)?)`)

func parseStoryMeta(body []byte, storyURL string, src *config.SourceConfig) (*StoryMeta, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("parse story html: %w", err)
	}

	meta := &StoryMeta{
		URL:       storyURL,
		StorySlug: storySlugFromURL(storyURL),
	}

	// Resolve selectors from SourceConfig with hardcoded defaults.
	titleSels := []string{"h1.book-name", "h1.story-title", ".book-name h1", "h1"}
	coverSel := "div.info-holder img"
	infoContainer := "div.info"
	authorLabel := "tác giả"
	genreLabel := "thể loại"
	statusLabel := "trạng thái"
	descSels := []string{"div.desc-text", "div.desc", ".book-intro", ".summary"}

	if src != nil {
		if len(src.Story.TitleSelectors) > 0 {
			titleSels = src.Story.TitleSelectors
		}
		if src.Story.CoverContainer != "" {
			coverSel = src.Story.CoverContainer
		}
		if src.Story.InfoContainer != "" {
			infoContainer = src.Story.InfoContainer
		}
		if src.Story.AuthorLabel != "" {
			authorLabel = src.Story.AuthorLabel
		}
		if src.Story.GenreLabel != "" {
			genreLabel = src.Story.GenreLabel
		}
		if src.Story.StatusLabel != "" {
			statusLabel = src.Story.StatusLabel
		}
		if len(src.Story.DescSelectors) > 0 {
			descSels = src.Story.DescSelectors
		}
	}

	// Title
	for _, sel := range titleSels {
		if t := strings.TrimSpace(doc.Find(sel).First().Text()); t != "" {
			meta.Title = t
			break
		}
	}

	// Cover image: data-pc > data-mb > src
	if img := doc.Find(coverSel).First(); img.Length() > 0 {
		for _, attr := range []string{"data-pc", "data-mb", "src"} {
			if v, exists := img.Attr(attr); exists && v != "" {
				meta.CoverImage = resolveURL(storyURL, v)
				break
			}
		}
	}

	// Author / genre / status from info container rows labelled by h3
	doc.Find(infoContainer + " div").Each(func(_ int, row *goquery.Selection) {
		h3 := row.Find("h3").First()
		if h3.Length() == 0 {
			return
		}
		label := strings.ToLower(strings.TrimSpace(h3.Text()))
		switch {
		case strings.Contains(label, authorLabel):
			if a := row.Find("a").First(); a.Length() > 0 {
				meta.Author = strings.TrimSpace(a.Text())
			}
		case strings.Contains(label, genreLabel):
			var genres []string
			row.Find("a").Each(func(_ int, a *goquery.Selection) {
				if g := strings.TrimSpace(a.Text()); g != "" {
					genres = append(genres, g)
				}
			})
			meta.Genre = strings.Join(genres, ", ")
		case strings.Contains(label, statusLabel):
			if span := row.Find("span").First(); span.Length() > 0 {
				meta.Status = strings.TrimSpace(span.Text())
			}
		}
	})

	// Rating — try common static selectors (JavaScript-rendered ratings won't appear here)
	meta.Rating = parseRatingFromDoc(doc)

	// Description
	for _, sel := range descSels {
		if tag := doc.Find(sel).First(); tag.Length() > 0 {
			raw := strings.TrimSpace(tag.Text())
			meta.Description = reDescPrefix.ReplaceAllString(raw, "")
			break
		}
	}

	return meta, nil
}

// parseRatingFromDoc extracts a 0–5 float rating from common static HTML patterns.
// Returns 0 if not found; caller should fall back to a default.
func parseRatingFromDoc(doc *goquery.Document) float64 {
	candidates := []string{
		"span.rate", "em.rate", "cite.rate",
		"span[itemprop='ratingValue']", "meta[itemprop='ratingValue']",
		"div.rate span", "div.rating span", "div.score span",
		"span.score", "em.score",
	}
	for _, sel := range candidates {
		node := doc.Find(sel).First()
		if node.Length() == 0 {
			continue
		}
		// Try content attribute first (for <meta> tags), then text
		raw := node.AttrOr("content", strings.TrimSpace(node.Text()))
		raw = strings.ReplaceAll(raw, ",", ".")
		if m := reRatingNum.FindString(raw); m != "" {
			if v, err := strconv.ParseFloat(m, 64); err == nil && v > 0 && v <= 10 {
				if v > 5 {
					v = v / 2 // normalize 0–10 → 0–5
				}
				return v
			}
		}
	}
	return 0
}

func extractChapterLinks(body []byte, baseURL string, chapterRe *regexp.Regexp, allowedDomain string) ([]ChapterRef, []string) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, nil
	}

	var chapters []ChapterRef
	var pages []string

	doc.Find("a[href]").Each(func(_ int, s *goquery.Selection) {
		href, ok := s.Attr("href")
		if !ok || href == "" {
			return
		}
		full := resolveURL(baseURL, href)
		parsed, err := url.Parse(full)
		if err != nil {
			return
		}
		if allowedDomain != "" && parsed.Host != allowedDomain {
			return
		}
		path := parsed.Path

		if m := chapterRe.FindStringSubmatch(path); m != nil {
			num := parseInt(m[1])
			// last segment without .html extension
			rawSeg := path[strings.LastIndex(path, "/")+1:]
			seg := strings.TrimSuffix(rawSeg, ".html")
			title := normalizeSpaces(s.Text())
			if title == "" {
				title = fmt.Sprintf("Chương %d", num)
			}
			chapters = append(chapters, ChapterRef{
				URL:    normalizeURL(full),
				Title:  title,
				Slug:   slugify(seg),
				Number: num,
			})
			return
		}

		if rePaginationPath.MatchString(path) {
			pages = append(pages, normalizeURL(full))
		}
	})

	return chapters, pages
}
