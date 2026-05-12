# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
make build        # compile → bin/fetcher
make run          # go run ./cmd/server/main.go
make test         # go test -race ./...
make lint         # go vet ./...
make tidy         # go mod tidy
make docker-build # docker build -t nightowl-fetcher:latest .

# Manual API smoke tests (server must be running)
make client-test                        # POST /fetch/listing (hardcoded URL)
make client-story URL=https://...       # POST /fetch/story
```

Run a single package's tests:
```bash
go test -race ./internal/parse/...
go test -race -run TestFuncName ./internal/db/...
```

## Architecture

**nightowl-fetcher** is a Go HTTP service that crawls Vietnamese novel sites, writes chapter content to disk as Markdown files, and syncs metadata to MySQL. It is a port of a Python scraper that lives in the sibling `../night-owl/` repo (FastAPI + Python scraper). The two services share the same MySQL database and `story-content/` directory on disk.

### Data flow

```
Listing pages → Parser → story URLs
Story URL     → Parser → StoryMeta + ChapterRefs
ChapterRefs   → Parser → Chapter.ContentMD (HTML→Markdown)
ContentMD     → disk:  story-content/<slug>/<NNNN>-<chapter-slug>.md
               → MySQL: books + chapters tables (via db.UpsertStoryFromDir)
```

### Package layout

| Package | Role |
|---|---|
| `cmd/server` | Wires everything, starts HTTP server + scheduler |
| `internal/config` | Two configs: `sources.yaml` (CSS selectors per domain) and `scrape_sources.json` (scheduler + scrape targets) |
| `internal/fetch` | Rate-limited HTTP client with UA rotation and exponential backoff retry |
| `internal/parse` | `Parser`: listing BFS, story meta extraction, chapter list, chapter HTML→Markdown |
| `internal/crawler` | `Crawler`: orchestrates the full per-story pipeline (listing → filter existing → fetch → write disk → upsert DB) |
| `internal/job` | `Scheduler`: runs sources on `continuous` / `interval` schedule with active_window gating |
| `internal/db` | MySQL pool init + `UpsertStoryFromDir` (port of Python `database.py`) + `GetExistingSlugs` |
| `internal/handler` | HTTP handlers streaming NDJSON responses |

### HTTP endpoints

All non-health endpoints stream **NDJSON** (one JSON object per line).

| Method | Path | Description |
|---|---|---|
| GET | `/health` | `{"status":"ok"}` |
| POST | `/fetch/listing` | Stream story URLs from a listing page |
| POST | `/fetch/story` | Stream `story_meta` + `chapter` objects for one story |
| POST | `/crawl/stories` | Full crawl (disk + DB) for a batch of URLs (max 50) |

### Config files

- **`sources.yaml`** — CSS selectors keyed by domain. Loaded at startup. Add a new domain block to support a new site without code changes.
- **`scrape_sources.json`** — Scheduler config: schedule type (`continuous`/`interval`), source list with `target_count`, `free_chapter_threshold`, `concurrency`, and `enabled` flag. `content_root` and `STORY_CONTENT_ROOT` env var control where chapters are saved.

### Key design details

- **Chapter dedup**: `crawler.crawlStory` reads existing `.md` files in the story dir to build a set of known chapter numbers, then only fetches missing ones. `db.UpsertStoryFromDir` uses `INSERT IGNORE` on `(book_id, chapter_number)`.
- **Chapter filename format**: `<NNNN>-<chapter-slug>.md` where NNNN is a sequential file index (not chapter number). Chapter number is parsed from the slug via `chuong-(\d+)` regex.
- **Concurrency layering**: global HTTP semaphore (`fetch.Client`) → per-source goroutine semaphore (`crawler.RunSource`) → per-chapter workers (fixed 4 in `fetchStory`).
- **Jitter**: both the scheduler and chapter fetcher add random delays (1.5–4s per story, 300–1300ms per chapter) to avoid synchronized bursts.
- **`recrawl_existing`**: when `false` (default), known slugs are skipped entirely. When `true` (forced by `continuous` mode), all stories are rechecked but chapter dedup still applies — only new chapter numbers are fetched and inserted.

### Planned work

`docs/incremental-crawl-strategy.md` proposes a 3-tier crawl refactor (status: **Proposed**):
- Tier 1 listing-only monitor (hourly, cheap)
- Tier 2 active updater for ongoing stories only
- Tier 3 monthly audit

Requires DB migration (`ALTER TABLE books ADD COLUMN last_checked_at DATETIME NULL, ADD COLUMN source_chapter_count INT NULL`) and new `RunListingOnly` / `RunActiveUpdate` crawler methods.

### Environment variables

| Var | Default | Purpose |
|---|---|---|
| `PORT` | `8080` | HTTP listen port |
| `CONCURRENCY` | `4` | Global HTTP concurrency cap |
| `MAX_RETRY` | `3` | HTTP retry count |
| `DB_HOST/PORT/USER/PASSWORD/NAME` | `localhost/3306/nightowl/nightowl/nightowl` | MySQL DSN |
| `STORY_CONTENT_ROOT` | `story-content` | Base dir for chapter Markdown files |
| `SCRAPE_SOURCES_PATH` | `scrape_sources.json` | Path to scheduler config |
| `LOG_FORMAT` | `json` | Set to anything else for console (colored) output |
