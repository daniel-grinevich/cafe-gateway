// Package openrouter is the upstream client: decisions, embeddings and the model catalogue.
//
// It retries 429/5xx and transport errors with exponential backoff and jitter. The upstream key never leaves this
// package and is never logged.
package openrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/daniel-grinevich/cafe-gateway/internal/config"
)

var retryStatuses = map[int]bool{429: true, 500: true, 502: true, 503: true, 504: true, 524: true, 529: true}

// maxResponseBytes caps what we read from upstream (embeddings for 256 inputs are a few MB).
const maxResponseBytes = 64 << 20

type Response struct {
	Status   int
	Body     json.RawMessage
	Attempts int
}

// ErrUnavailable is a transport-level failure after all retries.
var ErrUnavailable = errors.New("openrouter unavailable")

type Client struct {
	cfg  *config.Config
	http *http.Client
	// Sleep is replaceable in tests.
	Sleep func(context.Context, time.Duration) error
}

func New(cfg *config.Config, transport http.RoundTripper) *Client {
	if transport == nil {
		transport = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: cfg.UpstreamConnectTimeout, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   cfg.UpstreamConnectTimeout,
			ResponseHeaderTimeout: cfg.UpstreamReadTimeout,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			ForceAttemptHTTP2:     true,
		}
	}
	return &Client{
		cfg:   cfg,
		http:  &http.Client{Transport: transport, Timeout: cfg.UpstreamConnectTimeout + cfg.UpstreamReadTimeout},
		Sleep: sleep,
	}
}

func (c *Client) Configured() bool { return c.cfg.OpenRouterKey != "" }

func (c *Client) Decide(ctx context.Context, payload map[string]any) (*Response, error) {
	return c.post(ctx, c.cfg.OpenRouterDecisionsURL, payload)
}

func (c *Client) Embed(ctx context.Context, payload map[string]any) (*Response, error) {
	return c.post(ctx, c.cfg.OpenRouterBaseURL+"/embeddings", payload)
}

func (c *Client) newRequest(ctx context.Context, method, url string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HTTP-Referer", c.cfg.OpenRouterAppURL)
	req.Header.Set("X-Title", c.cfg.OpenRouterAppTitle)
	if c.cfg.OpenRouterKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.OpenRouterKey)
	}
	return req, nil
}

func (c *Client) post(ctx context.Context, url string, payload map[string]any) (*Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	for attempt := 1; ; attempt++ {
		req, err := c.newRequest(ctx, http.MethodPost, url, body)
		if err != nil {
			return nil, err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil || attempt > c.cfg.UpstreamMaxRetries {
				return nil, fmt.Errorf("%w: %s", ErrUnavailable, errorKind(err))
			}
			if err := c.Sleep(ctx, c.backoff(attempt, "")); err != nil {
				return nil, errors.Join(ErrUnavailable, err)
			}
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()
		if retryStatuses[resp.StatusCode] && attempt <= c.cfg.UpstreamMaxRetries {
			slog.WarnContext(ctx, "openrouter retry", "status", resp.StatusCode, "attempt", attempt)
			if err := c.Sleep(ctx, c.backoff(attempt, resp.Header.Get("Retry-After"))); err != nil {
				return nil, errors.Join(ErrUnavailable, err)
			}
			continue
		}
		if readErr != nil {
			return nil, fmt.Errorf("%w: reading body: %s", ErrUnavailable, errorKind(readErr))
		}
		if !json.Valid(raw) {
			msg, _ := json.Marshal(map[string]any{"error": map[string]any{"message": truncate(string(raw), 500)}})
			raw = msg
		}
		return &Response{Status: resp.StatusCode, Body: raw, Attempts: attempt}, nil
	}
}

// ListModels merges the general catalogue with the embeddings catalogue (embedding models aren't always in
// /models). Failures are logged and skipped.
func (c *Client) ListModels(ctx context.Context) []map[string]any {
	seen := map[string]bool{}
	var out []map[string]any
	for _, url := range []string{c.cfg.OpenRouterBaseURL + "/models?output_modalities=all", c.cfg.OpenRouterBaseURL + "/embeddings/models"} {
		req, err := c.newRequest(ctx, http.MethodGet, url, nil)
		if err != nil {
			continue
		}
		resp, err := c.http.Do(req)
		if err != nil {
			slog.WarnContext(ctx, "model catalogue fetch failed", "url", url, "error", errorKind(err))
			continue
		}
		var body struct {
			Data []map[string]any `json:"data"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 400 || err != nil {
			slog.WarnContext(ctx, "model catalogue fetch failed", "url", url, "status", resp.StatusCode)
			continue
		}
		for _, m := range body.Data {
			if id, ok := m["id"].(string); ok && !seen[id] {
				seen[id] = true
				out = append(out, m)
			}
		}
	}
	return out
}

func (c *Client) backoff(attempt int, retryAfter string) time.Duration {
	delay := c.cfg.UpstreamBackoffBase * time.Duration(1<<(attempt-1))
	if secs, err := strconv.ParseFloat(retryAfter, 64); err == nil {
		delay = max(delay, min(time.Duration(secs*float64(time.Second)), 10*time.Second))
	}
	return delay + time.Duration(rand.Float64()*0.25*float64(delay)) // #nosec G404 -- jitter, not security
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// errorKind describes a transport error without echoing URLs or headers.
func errorKind(err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	}
	if inner := errors.Unwrap(err); inner != nil {
		return fmt.Sprintf("%T", inner)
	}
	return fmt.Sprintf("%T", err)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
