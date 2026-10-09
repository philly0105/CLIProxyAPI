package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func observedClaude(id string, observedAt time.Time, signals map[string]string) *Auth {
	return &Auth{ID: id, Provider: "claude", Quota: QuotaState{ObservedAt: observedAt, Signals: signals}}
}

func pickExpiring(t *testing.T, provider string, auths ...*Auth) string {
	t.Helper()
	got, err := (&ExpiringFirstSelector{}).Pick(context.Background(), provider, "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil {
		t.Fatalf("Pick() auth = nil")
	}
	return got.ID
}

func unixString(ts time.Time) string { return strconv.FormatInt(ts.Unix(), 10) }

// pickByDeadline exercises the deadline ranking that Fable and non-Claude requests use.
func pickByDeadline(auths ...*Auth) string { return pickExpiringFirst(auths, time.Now()).ID }

func fiveHour(id string, now time.Time, utilization string, reset time.Time) *Auth {
	return observedClaude(id, now, map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": utilization,
		"Anthropic-Ratelimit-Unified-5h-Reset":       unixString(reset),
		"Anthropic-Ratelimit-Unified-7d-Reset":       unixString(now.Add(time.Hour)),
	})
}

func TestExpiringFirst_ClaudePrefersLowestFiveHourUsage(t *testing.T) {
	t.Parallel()
	now := time.Now()
	busy := fiveHour("a", now, "0.62", now.Add(2*time.Hour))
	idle := fiveHour("b", now, "0.18", now.Add(4*time.Hour))
	mid := fiveHour("c", now, "0.40", now.Add(time.Hour))
	if got := pickExpiring(t, "claude", busy, idle, mid); got != "b" {
		t.Fatalf("picked %q, want b (lowest 5h usage)", got)
	}
}

func TestExpiringFirst_ClaudeExpiredFiveHourWindowCountsAsEmpty(t *testing.T) {
	t.Parallel()
	now := time.Now()
	reset := fiveHour("a", now, "0.90", now.Add(-time.Minute))
	low := fiveHour("b", now, "0.05", now.Add(time.Hour))
	if got := pickExpiring(t, "claude", low, reset); got != "a" {
		t.Fatalf("picked %q, want a (its 5h window already reset)", got)
	}
}

func TestExpiringFirst_ClaudeFiveHourTiePrefersSoonerReset(t *testing.T) {
	t.Parallel()
	now := time.Now()
	later := fiveHour("a", now, "0.30", now.Add(4*time.Hour))
	sooner := fiveHour("b", now, "0.30", now.Add(time.Hour))
	if got := pickExpiring(t, "claude", later, sooner); got != "b" {
		t.Fatalf("picked %q, want b (same usage, resets sooner)", got)
	}
}

func TestExpiringFirst_ClaudeFiveHourProbeAndUnknownTiers(t *testing.T) {
	t.Parallel()
	now := time.Now()
	known := fiveHour("a", now, "0.50", now.Add(time.Hour))
	noReading := observedClaude("b", now, map[string]string{"Anthropic-Ratelimit-Unified-Status": "allowed"})
	if got := pickExpiring(t, "claude", noReading, known); got != "a" {
		t.Fatalf("picked %q, want a (b has no 5h reading)", got)
	}
	unobserved := &Auth{ID: "z", Provider: "claude"}
	if got := pickExpiring(t, "claude", known, noReading, unobserved); got != "z" {
		t.Fatalf("picked %q, want z (never observed, probe first)", got)
	}
}

func TestExpiringFirst_MixedPoolKeepsDeadlineRanking(t *testing.T) {
	t.Parallel()
	now := time.Now()
	claude := fiveHour("a", now, "0.90", now.Add(time.Hour))
	codex := &Auth{ID: "b", Provider: "codex", Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
		"X-Codex-Secondary-Reset-At": unixString(now.Add(48 * time.Hour)),
	}}}
	if got := pickExpiring(t, "mixed", codex, claude); got != "a" {
		t.Fatalf("picked %q, want a (mixed pools rank by deadline)", got)
	}
}

func TestExpiringFirst_PrefersSoonerWeeklyReset(t *testing.T) {
	t.Parallel()
	now := time.Now()
	later := observedClaude("a", now, map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unixString(now.Add(5 * 24 * time.Hour))})
	sooner := observedClaude("b", now, map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unixString(now.Add(12 * time.Hour))})
	if got := pickByDeadline(later, sooner); got != "b" {
		t.Fatalf("picked %q, want b (resets sooner)", got)
	}
}

func TestExpiringFirst_ProbesUnobservedQuotaProviderFirst(t *testing.T) {
	t.Parallel()
	now := time.Now()
	known := observedClaude("a", now, map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unixString(now.Add(time.Hour))})
	unobserved := &Auth{ID: "z", Provider: "claude"}
	if got := pickByDeadline(known, unobserved); got != "z" {
		t.Fatalf("picked %q, want z (never observed, probe first)", got)
	}
}

func TestExpiringFirst_KnownDeadlineBeatsObservedWithoutDeadline(t *testing.T) {
	t.Parallel()
	now := time.Now()
	noDeadline := observedClaude("a", now, map[string]string{"Anthropic-Ratelimit-Unified-Status": "allowed"})
	known := observedClaude("b", now, map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unixString(now.Add(6 * 24 * time.Hour))})
	if got := pickByDeadline(noDeadline, known); got != "b" {
		t.Fatalf("picked %q, want b (has a deadline)", got)
	}
}

func TestExpiringFirst_SubscriptionEndBeatsLaterReset(t *testing.T) {
	t.Parallel()
	now := time.Now()
	resetSoon := observedClaude("a", now, map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unixString(now.Add(3 * 24 * time.Hour))})
	expiring := observedClaude("b", now, map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unixString(now.Add(6 * 24 * time.Hour))})
	expiring.Metadata = map[string]any{MetadataSubscriptionExpiresAt: now.Add(24 * time.Hour).UTC().Format("2006-01-02")}
	if got := pickByDeadline(resetSoon, expiring); got != "b" {
		t.Fatalf("picked %q, want b (subscription ends within ~1 day)", got)
	}
}

func TestExpiringFirst_IgnoresPastDeadlines(t *testing.T) {
	t.Parallel()
	now := time.Now()
	stale := observedClaude("a", now, map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unixString(now.Add(-time.Hour))})
	stale.Metadata = map[string]any{MetadataSubscriptionExpiresAt: "2001-01-01"}
	future := observedClaude("b", now, map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unixString(now.Add(4 * 24 * time.Hour))})
	if got := pickByDeadline(stale, future); got != "b" {
		t.Fatalf("picked %q, want b (a's deadlines are in the past)", got)
	}
}

func TestExpiringFirst_CodexSecondaryWindow(t *testing.T) {
	t.Parallel()
	now := time.Now()
	resetAt := &Auth{ID: "a", Provider: "codex", Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
		"X-Codex-Secondary-Reset-At": unixString(now.Add(48 * time.Hour)),
	}}}
	resetAfter := &Auth{ID: "b", Provider: "codex", Quota: QuotaState{ObservedAt: now.Add(-time.Hour), Signals: map[string]string{
		"X-Codex-Secondary-Reset-After-Seconds": strconv.Itoa(int((10 * time.Hour).Seconds())),
	}}}
	if got := pickExpiring(t, "codex", resetAt, resetAfter); got != "b" {
		t.Fatalf("picked %q, want b (resets in ~9h)", got)
	}
}

func TestExpiringFirst_CodexIDTokenSubscriptionEnd(t *testing.T) {
	t.Parallel()
	now := time.Now()
	payload, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{
		"chatgpt_subscription_active_until": now.Add(2 * time.Hour).UTC().Format(time.RFC3339),
	}})
	token := "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
	plain := &Auth{ID: "a", Provider: "codex", Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
		"X-Codex-Secondary-Reset-At": unixString(now.Add(5 * 24 * time.Hour)),
	}}}
	ending := &Auth{ID: "b", Provider: "codex", Metadata: map[string]any{"id_token": token}, Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
		"X-Codex-Secondary-Reset-At": unixString(now.Add(5 * 24 * time.Hour)),
	}}}
	if got := pickExpiring(t, "codex", plain, ending); got != "b" {
		t.Fatalf("picked %q, want b (subscription active until +2h)", got)
	}
}

func TestExpiringFirst_FallsBackToFillFirstOrder(t *testing.T) {
	t.Parallel()
	if got := pickExpiring(t, "gemini", &Auth{ID: "c", Provider: "gemini"}, &Auth{ID: "a", Provider: "gemini"}, &Auth{ID: "b", Provider: "gemini"}); got != "a" {
		t.Fatalf("picked %q, want a (fill-first order)", got)
	}
}

func TestExpiringFirst_SkipsCoolingDownCredential(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cooling := observedClaude("a", now, map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unixString(now.Add(time.Hour))})
	cooling.Unavailable = true
	cooling.NextRetryAfter = now.Add(30 * time.Minute)
	cooling.Quota.Exceeded = true
	cooling.Quota.NextRecoverAt = now.Add(30 * time.Minute)
	next := observedClaude("b", now, map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": unixString(now.Add(5 * 24 * time.Hour))})
	if got := pickExpiring(t, "claude", cooling, next); got != "b" {
		t.Fatalf("picked %q, want b (a is cooling down)", got)
	}
}

func TestParseExpiryValue(t *testing.T) {
	t.Parallel()
	want := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, raw := range []any{"2026-11-01", "2026-11-01T00:00:00Z", strconv.FormatInt(want.Unix(), 10), float64(want.Unix()), float64(want.UnixMilli()), json.Number(strconv.FormatInt(want.Unix(), 10))} {
		got, ok := parseExpiryValue(raw)
		if !ok || !got.Equal(want) {
			t.Fatalf("parseExpiryValue(%v) = %v, %v; want %v", raw, got, ok, want)
		}
	}
	for _, raw := range []any{nil, "", "soon", float64(0), true} {
		if _, ok := parseExpiryValue(raw); ok {
			t.Fatalf("parseExpiryValue(%v) ok = true, want false", raw)
		}
	}
}
