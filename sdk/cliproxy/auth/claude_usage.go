package auth

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// ClaudeUsageWindow is one Claude quota window: utilization in percent (0-100) and its reset.
type ClaudeUsageWindow struct {
	Percent  float64   `json:"percent"`
	ResetsAt time.Time `json:"resets_at,omitzero"`
}

// ClaudeUsage holds the quota windows of a Claude OAuth credential. A nil window was not reported.
type ClaudeUsage struct {
	FiveHour *ClaudeUsageWindow `json:"five_hour,omitempty"`
	SevenDay *ClaudeUsageWindow `json:"seven_day,omitempty"`
	Fable    *ClaudeUsageWindow `json:"fable,omitempty"`
}

// Empty reports whether no window was found.
func (u ClaudeUsage) Empty() bool {
	return u.FiveHour == nil && u.SevenDay == nil && u.Fable == nil
}

// FetchClaudeUsage queries Anthropic's OAuth usage endpoint for a Claude credential.
func FetchClaudeUsage(ctx context.Context, auth *Auth) (ClaudeUsage, error) {
	if authAccessToken(auth) == "" {
		return ClaudeUsage{}, errors.New("claude usage: no access token")
	}
	body, errFetch := fetchClaudeUsageBody(ctx, auth)
	if errFetch != nil {
		return ClaudeUsage{}, errFetch
	}
	return parseClaudeUsage(body)
}

// parseClaudeUsage reads the five_hour and seven_day windows plus the Fable weekly window.
func parseClaudeUsage(body []byte) (ClaudeUsage, error) {
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return ClaudeUsage{}, errUnmarshal
	}
	usage := ClaudeUsage{
		FiveHour: usageWindow(payload["five_hour"]),
		SevenDay: usageWindow(payload["seven_day"]),
	}
	fable, errFable := parseFableUsage(body)
	if errFable == nil && fable.HasLimit {
		usage.Fable = &ClaudeUsageWindow{Percent: fable.Percent, ResetsAt: fable.ResetsAt}
	}
	return usage, nil
}

func usageWindow(raw any) *ClaudeUsageWindow {
	window, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	percent, ok := usageNumber(window["utilization"])
	if !ok {
		return nil
	}
	reset, _ := parseExpiryValue(window["resets_at"])
	return &ClaudeUsageWindow{Percent: percent, ResetsAt: reset}
}

// ClaudeHeaderUsage builds 5-hour and 7-day windows from the unified rate-limit headers last
// observed on auth. Header utilization is a 0-1 fraction.
func ClaudeHeaderUsage(auth *Auth) ClaudeUsage {
	if auth == nil {
		return ClaudeUsage{}
	}
	signals := make(map[string]string, len(auth.Quota.Signals))
	for key, value := range auth.Quota.Signals {
		signals[strings.ToLower(strings.TrimSpace(key))] = value
	}
	window := func(name string) *ClaudeUsageWindow {
		fraction, errParse := strconv.ParseFloat(strings.TrimSpace(signals["anthropic-ratelimit-unified-"+name+"-utilization"]), 64)
		if errParse != nil {
			return nil
		}
		reset, _ := parseExpiryValue(signals["anthropic-ratelimit-unified-"+name+"-reset"])
		return &ClaudeUsageWindow{Percent: fraction * 100, ResetsAt: reset}
	}
	return ClaudeUsage{FiveHour: window("5h"), SevenDay: window("7d")}
}
