// Package job runs scrape sources on a configurable schedule.
// Supports the same modes as Python APScheduler config:
// continuous, interval, cron (daily by hour/minute).
package job

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/nightowl/fetcher/internal/config"
	"github.com/nightowl/fetcher/internal/crawler"
)

// Scheduler runs scrape jobs on the configured schedule.
type Scheduler struct {
	cfg     *config.ScrapeConfig
	crawler *crawler.Crawler
}

// New creates a Scheduler.
func New(cfg *config.ScrapeConfig, c *crawler.Crawler) *Scheduler {
	return &Scheduler{cfg: cfg, crawler: c}
}

// Start launches the scheduling loop in a background goroutine.
// It exits when ctx is cancelled.
func (s *Scheduler) Start(ctx context.Context) {
	switch s.cfg.Schedule.Type {
	case "continuous":
		go s.runContinuous(ctx)
	default: // "interval" and "cron" both map to a ticker
		go s.runInterval(ctx)
	}
}

// runContinuous loops forever: run all sources → sleep idle_seconds → repeat.
// Port of Python run_continuous_scrape().
func (s *Scheduler) runContinuous(ctx context.Context) {
	idle := time.Duration(s.cfg.Schedule.IdleSeconds * float64(time.Second))
	if idle <= 0 {
		idle = 30 * time.Second
	}

	for iter := 1; ; iter++ {
		if ctx.Err() != nil {
			return
		}
		log.Info().Int("iteration", iter).Msg("continuous scrape start")
		s.runAllSources(ctx)
		log.Info().Int("iteration", iter).Msg("continuous scrape done")

		select {
		case <-ctx.Done():
			return
		case <-time.After(idle):
		}
	}
}

// runInterval runs all sources immediately then on a ticker.
// Port of Python APScheduler interval trigger.
func (s *Scheduler) runInterval(ctx context.Context) {
	d := intervalDuration(s.cfg.Schedule)

	// First run immediately
	if s.inActiveWindow() {
		s.runAllSources(ctx)
	}

	ticker := time.NewTicker(d)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.inActiveWindow() {
				log.Info().Msg("outside active_window — skip")
				continue
			}
			s.runAllSources(ctx)
		}
	}
}

// runAllSources executes every enabled source concurrently, bounded by SourceConcurrency.
func (s *Scheduler) runAllSources(ctx context.Context) {
	var enabled []config.ScrapeSource
	for _, src := range s.cfg.Sources {
		if src.Enabled {
			enabled = append(enabled, src)
		}
	}
	log.Info().
		Int("sources", len(enabled)).
		Int("concurrency", s.cfg.SourceConcurrency).
		Msg("running all enabled sources")

	sem := make(chan struct{}, s.cfg.SourceConcurrency)
	var wg sync.WaitGroup

	for _, src := range enabled {
		if ctx.Err() != nil {
			break
		}
		src := src
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := s.crawler.RunSource(ctx, src); err != nil {
				log.Error().Err(err).Str("url", src.URL).Msg("source error")
			}
		}()
	}
	wg.Wait()
}

// inActiveWindow mirrors Python _within_active_window.
// Returns true if current time falls in [start, end) — supports overnight windows.
func (s *Scheduler) inActiveWindow() bool {
	w := s.cfg.Schedule.ActiveWindow
	if w.Start == "" || w.End == "" {
		return true
	}
	startMin := parseHHMM(w.Start)
	endMin := parseHHMM(w.End)
	now := time.Now()
	nowMin := now.Hour()*60 + now.Minute()

	if startMin <= endMin {
		return nowMin >= startMin && nowMin < endMin
	}
	// Overnight window e.g. 22:00–06:00
	return nowMin >= startMin || nowMin < endMin
}

// parseHHMM converts "HH:MM" to total minutes since midnight.
func parseHHMM(s string) int {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0
	}
	h, _ := strconv.Atoi(parts[0])
	m, _ := strconv.Atoi(parts[1])
	return h*60 + m
}

func intervalDuration(s config.Schedule) time.Duration {
	d := time.Duration(s.Hours)*time.Hour + time.Duration(s.Minutes)*time.Minute
	if d <= 0 {
		d = 2 * time.Hour
	}
	return d
}
