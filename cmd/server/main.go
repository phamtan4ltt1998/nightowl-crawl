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

	// HTTP server (health + manual NDJSON endpoints)
	h := handler.New(parser)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", h.Health)
	mux.HandleFunc("POST /fetch/listing", h.FetchListing)
	mux.HandleFunc("POST /fetch/story", h.FetchStory)

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 10 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start crawl scheduler if scrape_sources.json is present
	scrapeCfg, err := config.LoadScrapeConfig("")
	if err != nil {
		log.Warn().Err(err).Msg("no scrape_sources.json — scheduler disabled")
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
			Msg("scheduler starting")

		c := crawler.New(parser)
		sched := job.New(scrapeCfg, c)
		sched.Start(ctx)
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
