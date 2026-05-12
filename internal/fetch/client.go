// Package fetch provides a rate-limited HTTP client with retry and UA rotation.
package fetch

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"time"

	"golang.org/x/sync/semaphore"
)

var userAgents = []string{
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64; rv:125.0) Gecko/20100101 Firefox/125.0",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:125.0) Gecko/20100101 Firefox/125.0",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7; rv:125.0) Gecko/20100101 Firefox/125.0",
}

// Client is a concurrency-limited HTTP client with exponential backoff retry.
type Client struct {
	http     *http.Client
	sem      *semaphore.Weighted
	maxRetry int
}

// New creates a Client with the given per-host concurrency cap and retry count.
func New(concurrency int64, maxRetry int) *Client {
	t := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: int(concurrency) * 2,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{
		http:     &http.Client{Timeout: 30 * time.Second, Transport: t},
		sem:      semaphore.NewWeighted(concurrency),
		maxRetry: maxRetry,
	}
}

// Get fetches url, respecting the concurrency limit and retrying on transient errors.
func (c *Client) Get(ctx context.Context, url string) ([]byte, error) {
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return nil, fmt.Errorf("semaphore acquire: %w", err)
	}
	defer c.sem.Release(1)

	var lastErr error
	for i := 0; i <= c.maxRetry; i++ {
		if i > 0 {
			if err := sleepBackoff(ctx, i); err != nil {
				return nil, err
			}
		}
		body, err := c.doGet(ctx, url)
		if err == nil {
			return body, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("get %s (retries=%d): %w", url, c.maxRetry, lastErr)
}

func (c *Client) doGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgents[rand.Intn(len(userAgents))])
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	req.Header.Set("Accept-Language", "vi-VN,vi;q=0.9,en;q=0.8")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("server error %d", resp.StatusCode)
	}
	if resp.StatusCode == 429 {
		return nil, fmt.Errorf("rate limited (429)")
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("client error %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

// sleepBackoff waits 2^(attempt-1)s ± 0–500ms jitter, capped at 16s.
func sleepBackoff(ctx context.Context, attempt int) error {
	base := time.Duration(1<<uint(attempt-1)) * time.Second
	if base > 16*time.Second {
		base = 16 * time.Second
	}
	jitter := time.Duration(rand.Intn(500)) * time.Millisecond
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(base + jitter):
		return nil
	}
}
