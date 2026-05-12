package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/nightowl/fetcher/internal/config"
	"github.com/nightowl/fetcher/internal/crawler"
	"github.com/nightowl/fetcher/internal/db"
	"github.com/nightowl/fetcher/internal/fetch"
	"github.com/nightowl/fetcher/internal/handler"
	"github.com/nightowl/fetcher/internal/job"
	"github.com/nightowl/fetcher/internal/parse"
)

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	if os.Getenv("LOG_FORMAT") != "json" {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	}

	// Load fetcher config (sources.yaml + env)
	cfg, err := config.Load("sources.yaml")
	if err != nil {
		log.Fatal().Err(err).Msg("load fetcher config")
	}

	// Connect to MySQL
	if err := db.Init(); err != nil {
		log.Fatal().Err(err).Msg("db init failed")
	}
	log.Info().Msg("mysql connected")

	// Build shared parser
	client := fetch.New(cfg.Concurrency, cfg.MaxRetry)
	parser := parse.New(client, cfg.Sources)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Build crawler (used by both scheduler and HTTP handler)
	var sharedCrawler *crawler.Crawler
	scrapeCfg, err := config.LoadScrapeConfig("")
	if err != nil {
		log.Warn().Err(err).Msg("no scrape_sources.json — scheduler disabled")
		// Still create a crawler with defaults for manual endpoint use
		sharedCrawler = crawler.New(parser, "story-content", false)
	} else {
		enabled := 0
		for _, s := range scrapeCfg.Sources {
			if s.Enabled {
				enabled++
			}
		}
		log.Info().
			Str("mode", scrapeCfg.Schedule.Type).
			Int("sources", enabled).
			Int("source_concurrency", scrapeCfg.SourceConcurrency).
			Msg("scheduler starting")

		sharedCrawler = crawler.New(parser, scrapeCfg.ContentRoot, scrapeCfg.RecrawlExisting)
		sched := job.New(scrapeCfg, sharedCrawler)
		sched.Start(ctx)
	}

	// HTTP server (health + manual NDJSON endpoints)
	h := handler.New(parser, sharedCrawler)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", h.Health)
	mux.HandleFunc("POST /fetch/listing", h.FetchListing)
	mux.HandleFunc("POST /fetch/story", h.FetchStory)
	mux.HandleFunc("POST /crawl/stories", h.CrawlStories)

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 10 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	// Start HTTP server
	go func() {
		log.Info().Str("port", cfg.Port).
			Int64("concurrency", cfg.Concurrency).
			Msg("nightowl-fetcher starting")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("server error")
		}
	}()

	// Wait for signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	cancel() // stop scheduler

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutCancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Error().Err(err).Msg("shutdown error")
	}
	log.Info().Msg("nightowl-fetcher stopped")
}
