package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
)

const (
	claudeUsageURL       = "https://api.anthropic.com/api/oauth/usage"
	fableUsageMaxAge     = 5 * time.Minute
	fableUsageRetryAfter = time.Minute
	fableUsageTimeout    = 15 * time.Second
)

// fableUsage is the Fable weekly window reported by Anthropic's OAuth usage endpoint.
type fableUsage struct {
	ResetsAt time.Time
	Percent  float64
	HasLimit bool
}

type fableUsageEntry struct {
	usage     fableUsage
	fetchedAt time.Time
	failedAt  time.Time
	fetching  bool
}

// fableUsageCache holds per-credential Fable usage, refreshed in the background.
type fableUsageCache struct {
	mu      sync.Mutex
	entries map[string]*fableUsageEntry
	fetch   func(ctx context.Context, auth *Auth) (fableUsage, error)
}

var defaultFableUsageCache = &fableUsageCache{
	entries: make(map[string]*fableUsageEntry),
	fetch:   fetchClaudeFableUsage,
}

// isFableModel reports whether a requested model belongs to the Fable family.
func isFableModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "fable")
}

// lookup returns the cached usage for auth and starts a background refresh when the entry is
// missing or stale. It never blocks on the network.
func (c *fableUsageCache) lookup(auth *Auth, now time.Time) (fableUsage, bool) {
	if c == nil || auth == nil || auth.ID == "" || !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") {
		return fableUsage{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[auth.ID]
	if entry == nil {
		entry = &fableUsageEntry{}
		c.entries[auth.ID] = entry
	}
	stale := entry.fetchedAt.IsZero() || now.Sub(entry.fetchedAt) >= fableUsageMaxAge
	retryable := entry.failedAt.IsZero() || now.Sub(entry.failedAt) >= fableUsageRetryAfter
	if stale && retryable && !entry.fetching && authAccessToken(auth) != "" {
		entry.fetching = true
		go c.refresh(auth.Clone())
	}
	if entry.fetchedAt.IsZero() {
		return fableUsage{}, false
	}
	return entry.usage, true
}

func (c *fableUsageCache) refresh(auth *Auth) {
	ctx, cancel := context.WithTimeout(context.Background(), fableUsageTimeout)
	defer cancel()
	usage, errFetch := c.fetch(ctx, auth)
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[auth.ID]
	if entry == nil {
		entry = &fableUsageEntry{}
		c.entries[auth.ID] = entry
	}
	entry.fetching = false
	if errFetch != nil {
		entry.failedAt = time.Now()
		return
	}
	entry.usage = usage
	entry.fetchedAt = time.Now()
	entry.failedAt = time.Time{}
}

func fetchClaudeFableUsage(ctx context.Context, auth *Auth) (fableUsage, error) {
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, claudeUsageURL, nil)
	if errReq != nil {
		return fableUsage{}, errReq
	}
	req.Header.Set("User-Agent", "claude-cli/2.1.280 (external, cli)")
	req.Header.Set("Authorization", "Bearer "+authAccessToken(auth))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	client := &http.Client{Timeout: fableUsageTimeout}
	if proxyStr := strings.TrimSpace(auth.ProxyURL); proxyStr != "" {
		if transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyStr); errBuild == nil && transport != nil {
			client.Transport = transport
		}
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return fableUsage{}, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return fableUsage{}, errRead
	}
	if resp.StatusCode != http.StatusOK {
		return fableUsage{}, fmt.Errorf("claude usage: status %d", resp.StatusCode)
	}
	return parseFableUsage(body)
}

// parseFableUsage extracts the Fable weekly window from a usage response. It prefers the active
// weekly_scoped limit whose model is Fable and falls back to the iguana_necktie window when that
// window is not a credit pool.
func parseFableUsage(body []byte) (fableUsage, error) {
	var payload struct {
		Limits []struct {
			Kind     string `json:"kind"`
			Percent  any    `json:"percent"`
			ResetsAt string `json:"resets_at"`
			IsActive bool   `json:"is_active"`
			Scope    struct {
				Model struct {
					DisplayName string `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		} `json:"limits"`
		IguanaNecktie map[string]any `json:"iguana_necktie"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return fableUsage{}, errUnmarshal
	}

	found := -1
	for i, limit := range payload.Limits {
		name := strings.ToLower(strings.TrimSpace(limit.Scope.Model.DisplayName))
		if _, hasPercent := usageNumber(limit.Percent); !hasPercent {
			continue
		}
		if strings.ToLower(strings.TrimSpace(limit.Kind)) != "weekly_scoped" || (name != "fable" && name != "fable 5") {
			continue
		}
		if found < 0 || (limit.IsActive && !payload.Limits[found].IsActive) {
			found = i
		}
	}
	if found >= 0 {
		limit := payload.Limits[found]
		usage := fableUsage{HasLimit: true}
		usage.ResetsAt, _ = parseExpiryValue(limit.ResetsAt)
		usage.Percent, _ = usageNumber(limit.Percent)
		return usage, nil
	}

	if window := payload.IguanaNecktie; window != nil && !isCreditPoolWindow(window) {
		usage := fableUsage{HasLimit: true}
		usage.ResetsAt, _ = parseExpiryValue(window["resets_at"])
		usage.Percent, _ = usageNumber(window["utilization"])
		return usage, nil
	}
	return fableUsage{}, nil
}

// isCreditPoolWindow reports whether an iguana_necktie window is the Pro/Max cloud-session
// credit pool rather than the Team Fable weekly limit. Dollar fields mark the credit pool.
func isCreditPoolWindow(window map[string]any) bool {
	for _, key := range []string{"limit_dollars", "used_dollars", "remaining_dollars"} {
		if _, ok := usageNumber(window[key]); ok {
			return true
		}
	}
	return false
}

func usageNumber(raw any) (float64, bool) {
	switch value := raw.(type) {
	case float64:
		return value, true
	case string:
		number, errParse := strconv.ParseFloat(strings.TrimSpace(value), 64)
		return number, errParse == nil
	default:
		return 0, false
	}
}
