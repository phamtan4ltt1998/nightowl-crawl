package parse

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// FetchStoryMeta fetches and parses metadata from a story's main page.
func (p *Parser) FetchStoryMeta(ctx context.Context, storyURL string) (*StoryMeta, error) {
	body, err := p.client.Get(ctx, storyURL)
	if err != nil {
		return nil, fmt.Errorf("fetch story meta: %w", err)
	}
	return parseStoryMeta(body, storyURL)
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

func parseStoryMeta(body []byte, storyURL string) (*StoryMeta, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("parse story html: %w", err)
	}

	meta := &StoryMeta{
		URL:       storyURL,
		StorySlug: storySlugFromURL(storyURL),
	}

	// Title: try multiple selectors, same as Python scraper
	for _, sel := range []string{"h1.book-name", "h1.story-title", ".book-name h1", "h1"} {
		if t := strings.TrimSpace(doc.Find(sel).First().Text()); t != "" {
			meta.Title = t
			break
		}
	}

	// Cover image: data-pc > data-mb > src
	if img := doc.Find("div.info-holder img").First(); img.Length() > 0 {
		for _, attr := range []string{"data-pc", "data-mb", "src"} {
			if v, exists := img.Attr(attr); exists && v != "" {
				meta.CoverImage = resolveURL(storyURL, v)
				break
			}
		}
	}

	// Author / genre / status from div.info rows labelled by h3
	reDescPrefix := regexp.MustCompile(`(?i)^Giới Thiệu\s*:\s*`)
	doc.Find("div.info div").Each(func(_ int, row *goquery.Selection) {
		h3 := row.Find("h3").First()
		if h3.Length() == 0 {
			return
		}
		label := strings.ToLower(strings.TrimSpace(h3.Text()))
		switch {
		case strings.Contains(label, "tác giả"):
			if a := row.Find("a").First(); a.Length() > 0 {
				meta.Author = strings.TrimSpace(a.Text())
			}
		case strings.Contains(label, "thể loại"):
			var genres []string
			row.Find("a").Each(func(_ int, a *goquery.Selection) {
				if g := strings.TrimSpace(a.Text()); g != "" {
					genres = append(genres, g)
				}
			})
			meta.Genre = strings.Join(genres, ", ")
		case strings.Contains(label, "trạng thái"):
			if span := row.Find("span").First(); span.Length() > 0 {
				meta.Status = strings.TrimSpace(span.Text())
			}
		}
	})

	// Description
	for _, sel := range []string{"div.desc-text", "div.desc", ".book-intro", ".summary"} {
		if tag := doc.Find(sel).First(); tag.Length() > 0 {
			raw := strings.TrimSpace(tag.Text())
			meta.Description = reDescPrefix.ReplaceAllString(raw, "")
			break
		}
	}

	return meta, nil
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
