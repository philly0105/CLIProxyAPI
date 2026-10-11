package auth

import (
	"testing"
	"time"
)

func TestParseClaudeUsageReadsAllWindows(t *testing.T) {
	body := []byte(`{
		"five_hour":{"utilization":18,"resets_at":"2026-10-11T20:00:00Z"},
		"seven_day":{"utilization":"62.5","resets_at":"2026-10-15T00:00:00Z"},
		"seven_day_opus":null,
		"limits":[{"kind":"weekly_scoped","scope":{"model":{"display_name":"Fable"}},"percent":40,"resets_at":"2026-10-14T00:00:00Z","is_active":true}]
	}`)
	usage, err := parseClaudeUsage(body)
	if err != nil {
		t.Fatalf("parseClaudeUsage: %v", err)
	}
	if usage.FiveHour == nil || usage.FiveHour.Percent != 18 || !usage.FiveHour.ResetsAt.Equal(time.Date(2026, 10, 11, 20, 0, 0, 0, time.UTC)) {
		t.Fatalf("five_hour = %+v", usage.FiveHour)
	}
	if usage.SevenDay == nil || usage.SevenDay.Percent != 62.5 {
		t.Fatalf("seven_day = %+v", usage.SevenDay)
	}
	if usage.Fable == nil || usage.Fable.Percent != 40 {
		t.Fatalf("fable = %+v", usage.Fable)
	}
}

func TestParseClaudeUsageMissingWindowsAreNil(t *testing.T) {
	usage, err := parseClaudeUsage([]byte(`{"five_hour":null}`))
	if err != nil {
		t.Fatalf("parseClaudeUsage: %v", err)
	}
	if !usage.Empty() {
		t.Fatalf("usage = %+v, want empty", usage)
	}
}

func TestClaudeHeaderUsageConvertsFractions(t *testing.T) {
	auth := &Auth{Quota: QuotaState{Signals: map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.25",
		"anthropic-ratelimit-unified-5h-reset":       "1791748800",
		"anthropic-ratelimit-unified-7d-utilization": "0.9",
	}}}
	usage := ClaudeHeaderUsage(auth)
	if usage.FiveHour == nil || usage.FiveHour.Percent != 25 || usage.FiveHour.ResetsAt.Unix() != 1791748800 {
		t.Fatalf("five_hour = %+v", usage.FiveHour)
	}
	if usage.SevenDay == nil || usage.SevenDay.Percent != 90 || !usage.SevenDay.ResetsAt.IsZero() {
		t.Fatalf("seven_day = %+v", usage.SevenDay)
	}
	if usage.Fable != nil {
		t.Fatalf("fable = %+v, want nil", usage.Fable)
	}
}
