package parse

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

var (
	rePaginationPath = regexp.MustCompile(`/trang-\d+/?$`)
	// Story path: /story-slug/ or /story-slug.12345/
	reStoryPath = regexp.MustCompile(`^/[a-z0-9][a-z0-9-]*(?:\.\d+)?/?$`)
)

// FetchListing crawls a listing URL (follows all pagination) and sends each
// discovered story URL to ch. Returns after all pages are visited.
func (p *Parser) FetchListing(ctx context.Context, listingURL string, ch chan<- string) error {
	visited := make(map[string]bool)
	queue := []string{normalizeURL(listingURL)}
	allowedDomain := domainOf(listingURL)
	seen := make(map[string]bool)

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

		stories, pages, err := extractListingLinks(body, listingURL, allowedDomain)
		if err != nil {
			continue
		}

		for _, s := range stories {
			if !seen[s] {
				seen[s] = true
				ch <- s
			}
		}
		for _, pg := range pages {
			if !visited[pg] {
				queue = append(queue, pg)
			}
		}
	}
	return nil
}

func extractListingLinks(body []byte, baseURL, allowedDomain string) (stories, pages []string, err error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, nil, fmt.Errorf("parse html: %w", err)
	}

	storySet := make(map[string]bool)
	pageSet := make(map[string]bool)

	doc.Find("a[href]").Each(func(_ int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists || href == "" {
			return
		}
		full := resolveURL(baseURL, href)
		if full == "" {
			return
		}
		parsed, err := url.Parse(full)
		if err != nil {
			return
		}
		if allowedDomain != "" && parsed.Host != allowedDomain {
			return
		}
		path := parsed.Path

		if rePaginationPath.MatchString(path) {
			pageSet[normalizeURL(full)] = true
			return
		}

		// Story pages: single-segment path with slug >3 chars containing "-"
		if reStoryPath.MatchString(path) {
			parts := nonEmptyParts(path)
			if len(parts) == 1 && len(parts[0]) > 3 && strings.Contains(parts[0], "-") {
				storySet[normalizeURL(full)] = true
			}
		}
	})

	for s := range storySet {
		stories = append(stories, s)
	}
	for pg := range pageSet {
		pages = append(pages, pg)
	}
	return stories, pages, nil
}
