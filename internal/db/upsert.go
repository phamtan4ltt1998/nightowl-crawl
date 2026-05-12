package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Port of Python database.py: _parse_chapter_number, upsert_story_from_dir, get_existing_slugs

var (
	reChapChuong  = regexp.MustCompile(`chuong-(\d+)`)
	reLeadDigits  = regexp.MustCompile(`^(\d+)`)
	reMdLink      = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
)

func parseChapterNumber(filename string) int {
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

func stripMdLink(s string) string {
	return strings.TrimSpace(reMdLink.ReplaceAllString(s, "$1"))
}

// UpsertArgs mirrors the parameters of Python's upsert_story_from_dir.
type UpsertArgs struct {
	Slug                 string
	StoryName            string
	FreeChapterThreshold int
	SourceURL            string
	Author               string
	Genre                string
	Status               string
	Description          string
	CoverImage           string
}

// UpsertResult mirrors the return value of Python's upsert_story_from_dir.
type UpsertResult struct {
	BookID        int64
	Slug          string
	NewChapters   int
	TotalChapters int
}

// UpsertStoryFromDir reads all *.md files under story-content/<slug>/ and
// syncs books + chapters to MySQL — exact port of Python upsert_story_from_dir.
func UpsertStoryFromDir(args UpsertArgs) (*UpsertResult, error) {
	contentRoot := os.Getenv("STORY_CONTENT_ROOT")
	if contentRoot == "" {
		contentRoot = "story-content"
	}
	storyDir := filepath.Join(contentRoot, args.Slug)

	entries, err := os.ReadDir(storyDir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", storyDir, err)
	}

	var chapterFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			chapterFiles = append(chapterFiles, e.Name())
		}
	}
	chapterCount := len(chapterFiles)

	// Build metadata defaults (Go has no BOOK_META dict — scraped data wins)
	title := args.StoryName
	if title == "" {
		title = strings.Title(strings.ReplaceAll(args.Slug, "-", " "))
	}
	author := args.Author
	if author == "" {
		author = "Không rõ"
	}
	genre := args.Genre
	if genre == "" {
		genre = "Tiên hiệp"
	}

	tx, err := pool.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var bookID int64
	existingNums := map[int]bool{}

	var existingID int64
	err = tx.QueryRow("SELECT id FROM books WHERE slug = ?", args.Slug).Scan(&existingID)
	switch {
	case err == sql.ErrNoRows:
		// INSERT new book
		res, err := tx.Exec(
			"INSERT INTO books (slug,title,author,genre,chapter_count,`reads`,rating,"+
				"c1,c2,emoji,description,tags,words,updated,source_url,cover_image,status)"+
				" VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
			args.Slug, title, author, genre,
			chapterCount, "0", 4.5,
			"#6941C6", "#9E77ED", "📖",
			args.Description, args.Status, "0",
			fmt.Sprintf("%d chương", chapterCount),
			args.SourceURL, args.CoverImage, args.Status,
		)
		if err != nil {
			return nil, fmt.Errorf("insert book: %w", err)
		}
		bookID, _ = res.LastInsertId()

	case err != nil:
		return nil, fmt.Errorf("select book: %w", err)

	default:
		// UPDATE existing book
		bookID = existingID
		sets := "chapter_count=?, updated=?"
		params := []any{chapterCount, fmt.Sprintf("%d chương", chapterCount)}

		if args.SourceURL != "" {
			sets += ", source_url=?"
			params = append(params, args.SourceURL)
		}
		if args.CoverImage != "" {
			sets += ", cover_image=?"
			params = append(params, args.CoverImage)
		}
		if args.Author != "" {
			sets += ", author=?"
			params = append(params, args.Author)
		}
		if args.Genre != "" {
			sets += ", genre=?"
			params = append(params, args.Genre)
		}
		if args.Status != "" {
			sets += ", status=?"
			params = append(params, args.Status)
		}
		if args.Description != "" {
			sets += ", description=?"
			params = append(params, args.Description)
		}
		params = append(params, bookID)
		if _, err := tx.Exec("UPDATE books SET "+sets+" WHERE id=?", params...); err != nil {
			return nil, fmt.Errorf("update book: %w", err)
		}

		// Load existing chapter numbers
		rows, err := tx.Query("SELECT chapter_number FROM chapters WHERE book_id=?", bookID)
		if err != nil {
			return nil, fmt.Errorf("select chapters: %w", err)
		}
		for rows.Next() {
			var n int
			_ = rows.Scan(&n)
			existingNums[n] = true
		}
		_ = rows.Close()
	}

	// Insert new chapters
	newCount := 0
	for _, fname := range chapterFiles {
		chNum := parseChapterNumber(fname)
		if chNum == 0 || existingNums[chNum] {
			continue
		}
		free := 1
		if args.FreeChapterThreshold > 0 && chNum > args.FreeChapterThreshold {
			free = 0
		}

		// Read chapter title from first line: "# Title"
		chTitle := fmt.Sprintf("Chương %d", chNum)
		filePath := filepath.Join(storyDir, fname)
		if raw, err := os.ReadFile(filePath); err == nil {
			if idx := strings.Index(string(raw), "\n"); idx > 0 {
				firstLine := strings.TrimSpace(string(raw[:idx]))
				if strings.HasPrefix(firstLine, "#") {
					t := strings.TrimSpace(strings.TrimLeft(firstLine, "#"))
					t = strings.Trim(stripMdLink(t), `"`)
					if t != "" {
						chTitle = t
					}
				}
			}
		}

		if _, err := tx.Exec(
			"INSERT IGNORE INTO chapters (book_id,chapter_number,title,file_path,free) VALUES (?,?,?,?,?)",
			bookID, chNum, chTitle, filePath, free,
		); err != nil {
			return nil, fmt.Errorf("insert chapter %d: %w", chNum, err)
		}
		newCount++
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return &UpsertResult{
		BookID:        bookID,
		Slug:          args.Slug,
		NewChapters:   newCount,
		TotalChapters: chapterCount,
	}, nil
}

// GetExistingSlugs returns the subset of slugs already in books table.
// Port of Python get_existing_slugs.
func GetExistingSlugs(slugs []string) (map[string]bool, error) {
	if len(slugs) == 0 {
		return map[string]bool{}, nil
	}
	ph := make([]string, len(slugs))
	args := make([]any, len(slugs))
	for i, s := range slugs {
		ph[i] = "?"
		args[i] = s
	}
	rows, err := pool.Query(
		"SELECT slug FROM books WHERE slug IN ("+strings.Join(ph, ",")+")",
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("get existing slugs: %w", err)
	}
	defer rows.Close()

	result := map[string]bool{}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		result[s] = true
	}
	return result, rows.Err()
}
