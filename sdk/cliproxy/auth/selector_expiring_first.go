package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// MetadataSubscriptionExpiresAt is the optional top-level auth JSON field that records when a
// credential's subscription ends. Accepted forms: RFC 3339 timestamp, YYYY-MM-DD date, or Unix
// seconds/milliseconds.
const MetadataSubscriptionExpiresAt = "subscription_expires_at"

// ExpiringFirstSelector burns the credential whose remaining quota will be lost soonest.
//
// Each available credential gets a deadline: the earliest of its subscription end and its
// long-window quota reset (Claude 7-day window, Codex secondary window, Devin weekly window).
// Unused quota disappears at that deadline, so the credential with the nearest deadline is
// used first and the others are saved for later. When it hits a rate limit, normal cooldown
// removes it from the candidates and the next-nearest deadline takes over.
//
// Ranking, from most to least preferred:
//  1. Credentials from quota-reporting providers that have not been observed yet. One request
//     teaches the selector their reset time, so they are probed before ranking settles.
//  2. Credentials with a known future deadline, nearest first.
//  3. Credentials with no known deadline, in fill-first order.
//
// Fable requests rank Claude credentials by their Fable weekly reset instead, read from
// Anthropic's usage endpoint in the background (see fableUsageCache). A credential whose Fable
// window is full drops to the last tier.
//
// Other Claude requests are spread instead: each goes to the credential with the lowest
// 5-hour utilization (see pickLeastFiveHourUsage).
//
// Ties keep the deterministic ID order used by FillFirstSelector.
type ExpiringFirstSelector struct{}

// Pick selects the available credential with the nearest quota-loss deadline.
func (s *ExpiringFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	if isFableModel(model) {
		return pickExpiringFirstWith(available, now, defaultFableUsageCache), nil
	}
	if allClaude(available) {
		return pickLeastFiveHourUsage(available, now), nil
	}
	return pickExpiringFirst(available, now), nil
}

func allClaude(auths []*Auth) bool {
	for _, auth := range auths {
		if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") {
			return false
		}
	}
	return len(auths) > 0
}

// pickLeastFiveHourUsage spreads non-Fable Claude traffic by sending each request to the
// credential with the lowest 5-hour window utilization. Never-observed credentials are probed
// first, credentials without a 5-hour reading go last, and ties prefer the window that resets
// sooner, then ID order.
func pickLeastFiveHourUsage(available []*Auth, now time.Time) *Auth {
	var best *Auth
	bestTier := math.MaxInt
	var bestUsage float64
	var bestReset time.Time
	for _, candidate := range available {
		tier, usage, reset := fiveHourRank(candidate, now)
		better := best == nil || tier < bestTier
		if !better && tier == bestTier && tier == expiringTierDeadline {
			better = usage < bestUsage || (usage == bestUsage && !reset.IsZero() && (bestReset.IsZero() || reset.Before(bestReset)))
		}
		if better {
			best, bestTier, bestUsage, bestReset = candidate, tier, usage, reset
		}
	}
	return best
}

// fiveHourRank reports a credential's tier, 5-hour utilization (0-1), and 5-hour reset. A
// window whose reset has passed counts as empty.
func fiveHourRank(auth *Auth, now time.Time) (int, float64, time.Time) {
	if auth.Quota.ObservedAt.IsZero() && len(auth.Quota.Signals) == 0 {
		return expiringTierProbe, 0, time.Time{}
	}
	var utilRaw, resetRaw string
	for key, value := range auth.Quota.Signals {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "anthropic-ratelimit-unified-5h-utilization":
			utilRaw = value
		case "anthropic-ratelimit-unified-5h-reset":
			resetRaw = value
		}
	}
	usage, errParse := strconv.ParseFloat(strings.TrimSpace(utilRaw), 64)
	if errParse != nil || math.IsNaN(usage) {
		return expiringTierUnknown, 0, time.Time{}
	}
	reset, ok := parseExpiryValue(resetRaw)
	if ok && !reset.After(now) {
		return expiringTierDeadline, 0, time.Time{}
	}
	if !ok {
		reset = time.Time{}
	}
	return expiringTierDeadline, usage, reset
}

const (
	expiringTierProbe = iota
	expiringTierDeadline
	expiringTierUnknown
)

func pickExpiringFirst(available []*Auth, now time.Time) *Auth {
	return pickExpiringFirstWith(available, now, nil)
}

// pickExpiringFirstWith ranks by Fable usage when fable is non-nil.
func pickExpiringFirstWith(available []*Auth, now time.Time, fable *fableUsageCache) *Auth {
	var best *Auth
	bestTier := math.MaxInt
	var bestDeadline time.Time
	for _, candidate := range available {
		tier, deadline := expiringFirstRank(candidate, now)
		if fable != nil {
			if usage, ok := fable.lookup(candidate, now); ok {
				tier, deadline = fableRank(candidate, usage, now, tier, deadline)
			}
		}
		if best == nil || tier < bestTier || (tier == bestTier && tier == expiringTierDeadline && deadline.Before(bestDeadline)) {
			best, bestTier, bestDeadline = candidate, tier, deadline
		}
	}
	return best
}

func expiringFirstRank(auth *Auth, now time.Time) (int, time.Time) {
	if auth == nil {
		return expiringTierUnknown, time.Time{}
	}
	if ProviderSupportsQuotaObservation(auth.Provider) && auth.Quota.ObservedAt.IsZero() && len(auth.Quota.Signals) == 0 {
		return expiringTierProbe, time.Time{}
	}
	if deadline, ok := ExpiringFirstDeadline(auth, now); ok {
		return expiringTierDeadline, deadline
	}
	return expiringTierUnknown, time.Time{}
}

// fableRank ranks a credential by its Fable weekly window, keeping the given rank when the
// usage response had no Fable window.
func fableRank(auth *Auth, usage fableUsage, now time.Time, tier int, deadline time.Time) (int, time.Time) {
	if !usage.HasLimit {
		return tier, deadline
	}
	if usage.Percent >= 100 {
		return expiringTierUnknown, time.Time{}
	}
	if !usage.ResetsAt.After(now) {
		return tier, deadline
	}
	best := usage.ResetsAt
	if end, ok := parseExpiryValue(auth.Metadata[MetadataSubscriptionExpiresAt]); ok && end.After(now) && end.Before(best) {
		best = end
	}
	return expiringTierDeadline, best
}

// ExpiringFirstDeadline returns the earliest future time at which the credential's unused quota
// is lost, or false when nothing is known.
func ExpiringFirstDeadline(auth *Auth, now time.Time) (time.Time, bool) {
	if auth == nil {
		return time.Time{}, false
	}
	var best time.Time
	consider := func(t time.Time, ok bool) {
		if ok && t.After(now) && (best.IsZero() || t.Before(best)) {
			best = t
		}
	}
	consider(parseExpiryValue(auth.Metadata[MetadataSubscriptionExpiresAt]))
	consider(codexSubscriptionActiveUntil(auth))
	for key, value := range auth.Quota.Signals {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "anthropic-ratelimit-unified-7d-reset", "x-codex-secondary-reset-at", "weekly_quota_reset_at", "plan_end":
			consider(parseExpiryValue(value))
		case "x-codex-secondary-reset-after-seconds":
			if seconds, errParse := strconv.ParseFloat(strings.TrimSpace(value), 64); errParse == nil && seconds > 0 && !auth.Quota.ObservedAt.IsZero() {
				consider(auth.Quota.ObservedAt.Add(time.Duration(seconds*float64(time.Second))), true)
			}
		}
	}
	return best, !best.IsZero()
}

// codexSubscriptionActiveUntil reads chatgpt_subscription_active_until from a Codex id_token.
func codexSubscriptionActiveUntil(auth *Auth) (time.Time, bool) {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return time.Time{}, false
	}
	idToken, _ := auth.Metadata["id_token"].(string)
	parts := strings.Split(strings.TrimSpace(idToken), ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, errDecode := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if errDecode != nil {
		return time.Time{}, false
	}
	var claims struct {
		Auth struct {
			ActiveUntil any `json:"chatgpt_subscription_active_until"`
		} `json:"https://api.openai.com/auth"`
	}
	if errUnmarshal := json.Unmarshal(payload, &claims); errUnmarshal != nil {
		return time.Time{}, false
	}
	return parseExpiryValue(claims.Auth.ActiveUntil)
}

// parseExpiryValue accepts RFC 3339 timestamps, YYYY-MM-DD dates (UTC midnight), and Unix
// seconds or milliseconds as strings or JSON numbers.
func parseExpiryValue(raw any) (time.Time, bool) {
	switch value := raw.(type) {
	case nil:
		return time.Time{}, false
	case string:
		value = strings.TrimSpace(value)
		if value == "" {
			return time.Time{}, false
		}
		if ts, errParse := time.Parse(time.RFC3339, value); errParse == nil {
			return ts, true
		}
		if ts, errParse := time.Parse("2006-01-02", value); errParse == nil {
			return ts, true
		}
		if number, errParse := strconv.ParseFloat(value, 64); errParse == nil {
			return unixExpiry(number)
		}
		return time.Time{}, false
	case float64:
		return unixExpiry(value)
	case json.Number:
		number, errParse := value.Float64()
		if errParse != nil {
			return time.Time{}, false
		}
		return unixExpiry(number)
	case int:
		return unixExpiry(float64(value))
	case int64:
		return unixExpiry(float64(value))
	default:
		return time.Time{}, false
	}
}

func unixExpiry(number float64) (time.Time, bool) {
	if number <= 0 || math.IsNaN(number) || math.IsInf(number, 0) {
		return time.Time{}, false
	}
	if number > 1e12 {
		return time.UnixMilli(int64(number)).UTC(), true
	}
	return time.Unix(int64(number), 0).UTC(), true
}
