package parse

import (
	"context"
	"fmt"
	"strings"

	md "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/PuerkitoBio/goquery"
)

// FetchChapter fetches a chapter URL and returns its markdown content.
func (p *Parser) FetchChapter(ctx context.Context, ref ChapterRef) (*Chapter, error) {
	body, err := p.client.Get(ctx, ref.URL)
	if err != nil {
		return nil, fmt.Errorf("fetch chapter %d: %w", ref.Number, err)
	}

	sels := p.contentSelectorsFor(ref.URL)
	content, title, err := extractChapterContent(body, ref.URL, sels)
	if err != nil || strings.TrimSpace(content) == "" {
		content = fmt.Sprintf("Không trích xuất được nội dung chương %d.", ref.Number)
	}

	if title == "" {
		title = ref.Title
	}

	content = replaceBranding(content)
	finalMD := fmt.Sprintf("# %s\n\n%s", title, strings.TrimSpace(content))

	return &Chapter{
		URL:       ref.URL,
		Title:     title,
		Number:    ref.Number,
		Slug:      ref.Slug,
		ContentMD: finalMD,
	}, nil
}

func extractChapterContent(body []byte, pageURL string, sels chapterSels) (content, title string, err error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return "", "", fmt.Errorf("parse chapter html: %w", err)
	}

	// Extract title from page
	for _, sel := range sels.title {
		if node := doc.Find(sel).First(); node.Length() > 0 {
			if t := normalizeSpaces(node.Text()); t != "" {
				title = t
				break
			}
		}
	}

	// Extract content HTML using first matching selector
	var contentHTML string
	for _, sel := range sels.content {
		if node := doc.Find(sel).First(); node.Length() > 0 {
			h, htmlErr := node.Html()
			if htmlErr == nil && strings.TrimSpace(h) != "" {
				contentHTML = h
				break
			}
		}
	}

	if contentHTML == "" {
		return "", title, nil
	}

	// Convert HTML → Markdown
	converter := md.NewConverter(pageURL, true, nil)
	markdown, convErr := converter.ConvertString(contentHTML)
	if convErr != nil {
		// Fallback: extract paragraph text
		var sb strings.Builder
		for _, sel := range sels.content {
			if node := doc.Find(sel).First(); node.Length() > 0 {
				node.Find("p").Each(func(_ int, p *goquery.Selection) {
					if t := strings.TrimSpace(p.Text()); t != "" {
						sb.WriteString(t)
						sb.WriteString("\n\n")
					}
				})
				break
			}
		}
		return strings.TrimSpace(sb.String()), title, nil
	}

	return strings.TrimSpace(markdown), title, nil
}

func replaceBranding(s string) string {
	s = strings.ReplaceAll(s, "Truyencom.com", "nightowl.com")
	s = strings.ReplaceAll(s, "truyencom.com", "nightowl.com")
	return s
}
