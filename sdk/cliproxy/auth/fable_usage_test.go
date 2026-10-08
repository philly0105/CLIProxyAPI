package auth

import (
	"context"
	"testing"
	"time"
)

func TestParseFableUsagePrefersActiveWeeklyScopedLimit(t *testing.T) {
	body := []byte(`{"limits":[
		{"kind":"weekly_scoped","scope":{"model":{"display_name":"Fable"}},"percent":10,"resets_at":"2026-10-20T00:00:00Z","is_active":false},
		{"kind":"weekly_scoped","scope":{"model":{"display_name":"Opus"}},"percent":90,"resets_at":"2026-10-09T00:00:00Z","is_active":true},
		{"kind":"weekly_scoped","scope":{"model":{"display_name":"fable 5"}},"percent":42.5,"resets_at":"2026-10-09T01:00:00Z","is_active":true}
	],"iguana_necktie":{"utilization":5,"resets_at":"2026-10-30T00:00:00Z"}}`)
	usage, err := parseFableUsage(body)
	if err != nil {
		t.Fatalf("parseFableUsage: %v", err)
	}
	if !usage.HasLimit || usage.Percent != 42.5 || !usage.ResetsAt.Equal(time.Date(2026, 10, 9, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("usage = %+v, want active fable 5 limit", usage)
	}
}

func TestParseFableUsageFallsBackToIguanaNecktie(t *testing.T) {
	usage, err := parseFableUsage([]byte(`{"iguana_necktie":{"utilization":"30","resets_at":"2026-10-13T04:59:00Z"}}`))
	if err != nil {
		t.Fatalf("parseFableUsage: %v", err)
	}
	if !usage.HasLimit || usage.Percent != 30 || !usage.ResetsAt.Equal(time.Date(2026, 10, 13, 4, 59, 0, 0, time.UTC)) {
		t.Fatalf("usage = %+v, want iguana_necktie window", usage)
	}
}

func TestParseFableUsageIgnoresCreditPool(t *testing.T) {
	usage, err := parseFableUsage([]byte(`{"iguana_necktie":{"utilization":30,"resets_at":"2026-10-13T04:59:00Z","limit_dollars":50}}`))
	if err != nil {
		t.Fatalf("parseFableUsage: %v", err)
	}
	if usage.HasLimit {
		t.Fatalf("usage = %+v, want no Fable window for a credit pool", usage)
	}
}

func seededFableCache(now time.Time, usages map[string]fableUsage) *fableUsageCache {
	cache := &fableUsageCache{
		entries: make(map[string]*fableUsageEntry),
		fetch: func(context.Context, *Auth) (fableUsage, error) {
			return fableUsage{}, nil
		},
	}
	for id, usage := range usages {
		cache.entries[id] = &fableUsageEntry{usage: usage, fetchedAt: now}
	}
	return cache
}

func TestPickExpiringFirstFablePrefersSoonestFableReset(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	team := &Auth{ID: "a-team", Provider: "claude", Quota: QuotaState{ObservedAt: now, Signals: map[string]string{"Anthropic-Ratelimit-Unified-5h-Reset": "1791500000"}}}
	pro := &Auth{ID: "b-pro", Provider: "claude", Quota: QuotaState{ObservedAt: now, Signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": "1791000000"}}}
	soonest := &Auth{ID: "c-soonest", Provider: "claude"}
	cache := seededFableCache(now, map[string]fableUsage{
		"a-team":    {HasLimit: true, Percent: 10, ResetsAt: now.Add(5 * 24 * time.Hour)},
		"b-pro":     {HasLimit: true, Percent: 10, ResetsAt: now.Add(4 * 24 * time.Hour)},
		"c-soonest": {HasLimit: true, Percent: 80, ResetsAt: now.Add(13 * time.Hour)},
	})

	got := pickExpiringFirstWith([]*Auth{team, pro, soonest}, now, cache)
	if got != soonest {
		t.Fatalf("picked %s, want c-soonest", got.ID)
	}
}

func TestPickExpiringFirstFableSkipsFullWindow(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	full := &Auth{ID: "a-full", Provider: "claude"}
	next := &Auth{ID: "b-next", Provider: "claude"}
	cache := seededFableCache(now, map[string]fableUsage{
		"a-full": {HasLimit: true, Percent: 100, ResetsAt: now.Add(time.Hour)},
		"b-next": {HasLimit: true, Percent: 50, ResetsAt: now.Add(48 * time.Hour)},
	})

	got := pickExpiringFirstWith([]*Auth{full, next}, now, cache)
	if got != next {
		t.Fatalf("picked %s, want b-next", got.ID)
	}
}

func TestFableUsageLookupFetchesInBackground(t *testing.T) {
	done := make(chan struct{})
	reset := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	cache := &fableUsageCache{
		entries: make(map[string]*fableUsageEntry),
		fetch: func(_ context.Context, auth *Auth) (fableUsage, error) {
			defer close(done)
			if authAccessToken(auth) != "token" {
				t.Errorf("access token = %q", authAccessToken(auth))
			}
			return fableUsage{HasLimit: true, ResetsAt: reset}, nil
		},
	}
	auth := &Auth{ID: "a", Provider: "claude", Metadata: map[string]any{"access_token": "token"}}

	if _, ok := cache.lookup(auth, time.Now()); ok {
		t.Fatal("first lookup returned data before any fetch")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("background fetch did not run")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if usage, ok := cache.lookup(auth, time.Now()); ok {
			if !usage.ResetsAt.Equal(reset) {
				t.Fatalf("ResetsAt = %v, want %v", usage.ResetsAt, reset)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("cached usage never appeared")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFableUsageLookupIgnoresNonClaude(t *testing.T) {
	cache := seededFableCache(time.Now(), nil)
	if _, ok := cache.lookup(&Auth{ID: "x", Provider: "codex", Metadata: map[string]any{"access_token": "t"}}, time.Now()); ok {
		t.Fatal("lookup returned data for a non-Claude credential")
	}
	if len(cache.entries) != 0 {
		t.Fatal("lookup created an entry for a non-Claude credential")
	}
}
