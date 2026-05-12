package config

import (
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Port           string
	Concurrency    int64
	MaxRetry       int
	ChromePoolSize int
	Sources        []SourceConfig
}

type SourceConfig struct {
	Domain  string           `yaml:"domain"`
	Story   StorySelectors   `yaml:"story"`
	Chapter ChapterSelectors `yaml:"chapter"`
}

type StorySelectors struct {
	TitleSelectors []string `yaml:"title_selectors"`
	CoverContainer string   `yaml:"cover_container"`
	InfoContainer  string   `yaml:"info_container"`
	DescSelectors  []string `yaml:"desc_selectors"`
	AuthorLabel    string   `yaml:"author_label"`
	GenreLabel     string   `yaml:"genre_label"`
	StatusLabel    string   `yaml:"status_label"`
}

type ChapterSelectors struct {
	ContentSelectors []string `yaml:"content_selectors"`
	TitleSelectors   []string `yaml:"title_selectors"`
}

type sourcesFile struct {
	Sources []SourceConfig `yaml:"sources"`
}

func Load(sourcesPath string) (*Config, error) {
	cfg := &Config{
		Port:           env("PORT", "8080"),
		Concurrency:    envInt64("CONCURRENCY", 4),
		MaxRetry:       envInt("MAX_RETRY", 3),
		ChromePoolSize: envInt("CHROME_POOL_SIZE", 2),
	}

	data, err := os.ReadFile(sourcesPath)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read %s: %w", sourcesPath, err)
	}

	var f sourcesFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse sources yaml: %w", err)
	}
	cfg.Sources = f.Sources
	return cfg, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}
