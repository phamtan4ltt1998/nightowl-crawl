package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// ScrapeSource mirrors one entry in scrape_sources.json["sources"].
type ScrapeSource struct {
	Genre                string `json:"_genre"`
	URL                  string `json:"url"`
	TargetCount          int    `json:"target_count"`
	FreeChapterThreshold int    `json:"free_chapter_threshold"`
	Concurrency          int    `json:"concurrency"`
	Enabled              bool   `json:"enabled"`
}

// ActiveWindow is the daily time window [Start, End) in HH:MM format.
type ActiveWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// Schedule mirrors scrape_sources.json["schedule"].
type Schedule struct {
	Type         string       `json:"type"`          // continuous | interval | cron
	IdleSeconds  float64      `json:"idle_seconds"`  // continuous only
	Hours        int          `json:"hours"`         // interval
	Minutes      int          `json:"minutes"`       // interval
	ActiveWindow ActiveWindow `json:"active_window"`
}

// ScrapeConfig is the full scrape_sources.json structure.
type ScrapeConfig struct {
	ContentRoot        string         `json:"content_root"`        // base dir for saved chapters; overrides STORY_CONTENT_ROOT env
	RecrawlExisting    bool           `json:"recrawl_existing"`    // re-check known stories for new chapters each pass
	SourceConcurrency  int            `json:"source_concurrency"`  // how many sources run in parallel (default 2)
	Schedule           Schedule       `json:"schedule"`
	Sources            []ScrapeSource `json:"sources"`
}

// LoadScrapeConfig reads scrape_sources.json.
// Path resolution order:
//  1. path argument (if non-empty)
//  2. SCRAPE_SOURCES_PATH env var
//  3. ./scrape_sources.json (cwd)
func LoadScrapeConfig(path string) (*ScrapeConfig, error) {
	if path == "" {
		path = os.Getenv("SCRAPE_SOURCES_PATH")
	}
	if path == "" {
		path = "scrape_sources.json"
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var cfg ScrapeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse scrape config: %w", err)
	}

	// Resolve content_root: JSON → env → default
	if cfg.ContentRoot == "" {
		if v := os.Getenv("STORY_CONTENT_ROOT"); v != "" {
			cfg.ContentRoot = v
		} else {
			cfg.ContentRoot = "story-content"
		}
	}

	// continuous mode implicitly recrawls existing stories for new chapters
	if cfg.Schedule.Type == "continuous" && !cfg.RecrawlExisting {
		cfg.RecrawlExisting = true
	}
	if cfg.SourceConcurrency <= 0 {
		cfg.SourceConcurrency = 2
	}

	// Apply defaults
	if cfg.Schedule.Type == "" {
		cfg.Schedule.Type = "interval"
	}
	if cfg.Schedule.IdleSeconds == 0 {
		cfg.Schedule.IdleSeconds = 30
	}
	if cfg.Schedule.Hours == 0 && cfg.Schedule.Minutes == 0 &&
		cfg.Schedule.Type == "interval" {
		cfg.Schedule.Hours = 2
	}

	return &cfg, nil
}
