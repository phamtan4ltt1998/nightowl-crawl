// Package parse crawls Vietnamese novel listing and story pages.
package parse

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/nightowl/fetcher/internal/config"
	"github.com/nightowl/fetcher/internal/fetch"
)

// Parser orchestrates listing, story, and chapter crawling.
type Parser struct {
	client  *fetch.Client
	sources []config.SourceConfig
}

// New creates a Parser.
func New(client *fetch.Client, sources []config.SourceConfig) *Parser {
	return &Parser{client: client, sources: sources}
}

// StoryMeta holds metadata for a story page.
type StoryMeta struct {
	URL         string  `json:"url"`
	Title       string  `json:"title"`
	Author      string  `json:"author"`
	Genre       string  `json:"genre"`
	Status      string  `json:"status"`
	Description string  `json:"description"`
	CoverImage  string  `json:"cover_image"`
	StorySlug   string  `json:"story_slug"`
	Rating      float64 `json:"rating"` // 0 means not found on page; caller should fallback to 4.5
}

// ChapterRef is a discovered link to a chapter.
type ChapterRef struct {
	URL    string `json:"url"`
	Title  string `json:"title"`
	Slug   string `json:"slug"`
	Number int    `json:"number"`
}

// Chapter is a fully-fetched chapter with markdown content.
type Chapter struct {
	URL       string `json:"url"`
	Title     string `json:"title"`
	Number    int    `json:"number"`
	Slug      string `json:"slug"`
	ContentMD string `json:"content_md"`
}

// chapterSels holds resolved CSS selectors for chapter content.
type chapterSels struct {
	content []string
	title   []string
}

var (
	defaultContentSelectors = []string{
		"div.chapter-content",
		"div#chapter-c",
		"div.box-chap",
		"div.content-chapter",
		"div#chapter-content",
		"article.chapter",
		"div.text-chapter",
	}
	defaultTitleSelectors = []string{
		"h2.chapter-title",
		"h1.chapter-title",
		".chapter-title",
		"h2",
	}
)

// sourceConfigFor returns the SourceConfig matching the URL's domain, or zero value.
func (p *Parser) sourceConfigFor(rawURL string) *config.SourceConfig {
	domain := domainOf(rawURL)
	for i := range p.sources {
		if p.sources[i].Domain == domain {
			return &p.sources[i]
		}
	}
	return nil
}

// contentSelectorsFor returns chapter CSS selectors for the given URL's domain.
func (p *Parser) contentSelectorsFor(rawURL string) chapterSels {
	domain := domainOf(rawURL)
	for i := range p.sources {
		if p.sources[i].Domain == domain {
			s := &p.sources[i]
			cs := chapterSels{
				content: defaultContentSelectors,
				title:   defaultTitleSelectors,
			}
			if len(s.Chapter.ContentSelectors) > 0 {
				cs.content = s.Chapter.ContentSelectors
			}
			if len(s.Chapter.TitleSelectors) > 0 {
				cs.title = s.Chapter.TitleSelectors
			}
			return cs
		}
	}
	return chapterSels{content: defaultContentSelectors, title: defaultTitleSelectors}
}

// --- URL helpers ---

func domainOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Host
}

func resolveURL(base, href string) string {
	if href == "" {
		return ""
	}
	b, err := url.Parse(base)
	if err != nil {
		return ""
	}
	ref, err := url.Parse(href)
	if err != nil {
		return ""
	}
	return b.ResolveReference(ref).String()
}

func normalizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Fragment = ""
	u.RawQuery = ""
	s := u.String()
	lastSeg := u.Path[strings.LastIndex(u.Path, "/")+1:]
	if u.Path != "" && !strings.Contains(lastSeg, ".") && !strings.HasSuffix(s, "/") {
		s += "/"
	}
	return s
}

var reStripNumSuffix = regexp.MustCompile(`\.\d+$`)

func storyKeyFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	parts := nonEmptyParts(u.Path)
	if len(parts) == 0 {
		return ""
	}
	seg := parts[len(parts)-1]
	if seg == "full" && len(parts) >= 2 {
		seg = parts[len(parts)-2]
	}
	return reStripNumSuffix.ReplaceAllString(seg, "")
}

func storySlugFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "story"
	}
	parts := nonEmptyParts(u.Path)
	if len(parts) == 0 {
		return "story"
	}
	seg := parts[len(parts)-1]
	if (seg == "full" || strings.HasPrefix(seg, "trang-")) && len(parts) >= 2 {
		seg = parts[len(parts)-2]
	}
	seg = reStripNumSuffix.ReplaceAllString(seg, "")
	return slugify(seg)
}

func nonEmptyParts(path string) []string {
	var out []string
	for _, p := range strings.Split(path, "/") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func slugify(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		case unicode.IsSpace(r) || r == '-' || r == '_':
			b.WriteRune('-')
		}
	}
	result := regexp.MustCompile(`-+`).ReplaceAllString(b.String(), "-")
	result = strings.Trim(result, "-")
	if result == "" {
		return "item"
	}
	return result
}

// StorySlugFromURL is the exported version of storySlugFromURL for use by
// other packages (e.g. crawler).
func StorySlugFromURL(rawURL string) string { return storySlugFromURL(rawURL) }

func normalizeSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func parseInt(s string) int {
	var n int
	fmt.Sscanf(s, "%d", &n)
	return n
}

func sortChapterRefs(m map[string]ChapterRef) []ChapterRef {
	refs := make([]ChapterRef, 0, len(m))
	for _, r := range m {
		refs = append(refs, r)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Number != refs[j].Number {
			return refs[i].Number < refs[j].Number
		}
		return refs[i].Slug < refs[j].Slug
	})
	return refs
}
