package quotahistory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func newTestRecorder(t *testing.T, path string, auths []*coreauth.Auth, now *time.Time) *Recorder {
	t.Helper()
	r := New(path, func() []*coreauth.Auth { return auths })
	r.now = func() time.Time { return *now }
	r.fetch = func(_ context.Context, auth *coreauth.Auth) (coreauth.ClaudeUsage, error) {
		if auth.ID == "broken" {
			return coreauth.ClaudeUsage{}, errors.New("status 401")
		}
		return coreauth.ClaudeUsage{SevenDay: &coreauth.ClaudeUsageWindow{Percent: 30, ResetsAt: now.Add(48 * time.Hour)}}, nil
	}
	return r
}

func TestSampleAllRecordsClaudeOnlyAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota-history.jsonl")
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	auths := []*coreauth.Auth{
		{ID: "a", Provider: "claude"},
		{ID: "off", Provider: "claude", Disabled: true},
		{ID: "codex", Provider: "codex"},
	}
	r := newTestRecorder(t, path, auths, &now)
	r.SampleAll(context.Background())

	samples, latest := r.Snapshot(time.Time{})
	if len(samples) != 1 || samples[0].ID != "a" || samples[0].SevenDay.Percent != 30 {
		t.Fatalf("samples = %+v", samples)
	}
	if latest["a"].Source != "usage" || len(latest) != 1 {
		t.Fatalf("latest = %+v", latest)
	}

	reloaded := newTestRecorder(t, path, auths, &now)
	reloaded.load()
	samples, latest = reloaded.Snapshot(time.Time{})
	if len(samples) != 1 || !samples[0].At.Equal(now) || latest["a"].Usage.SevenDay == nil {
		t.Fatalf("reloaded samples = %+v latest = %+v", samples, latest)
	}
}

func TestSampleFallsBackToHeadersOnFetchError(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	auths := []*coreauth.Auth{{ID: "broken", Provider: "claude", Quota: coreauth.QuotaState{Signals: map[string]string{
		"anthropic-ratelimit-unified-5h-utilization": "0.5",
	}}}}
	r := newTestRecorder(t, "", auths, &now)
	r.SampleAll(context.Background())

	samples, latest := r.Snapshot(time.Time{})
	status := latest["broken"]
	if len(samples) != 1 || samples[0].FiveHour == nil || samples[0].FiveHour.Percent != 50 {
		t.Fatalf("samples = %+v", samples)
	}
	if status.Source != "headers" || !strings.Contains(status.Error, "401") {
		t.Fatalf("status = %+v", status)
	}
}

func TestSampleWithNoDataKeepsPreviousUsage(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	auths := []*coreauth.Auth{{ID: "broken", Provider: "claude"}}
	r := newTestRecorder(t, "", auths, &now)
	r.latest["broken"] = Status{Usage: coreauth.ClaudeUsage{SevenDay: &coreauth.ClaudeUsageWindow{Percent: 10}}, FetchedAt: now.Add(-time.Hour)}
	r.SampleAll(context.Background())

	samples, latest := r.Snapshot(time.Time{})
	if len(samples) != 0 {
		t.Fatalf("samples = %+v, want none", samples)
	}
	if latest["broken"].Usage.SevenDay == nil || latest["broken"].Error == "" {
		t.Fatalf("latest = %+v", latest["broken"])
	}
}

func TestLoadDropsExpiredSamplesAndRewrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota-history.jsonl")
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	content := `{"t":"2026-09-01T00:00:00Z","id":"a","seven_day":{"percent":5}}` + "\n" +
		`not json` + "\n" +
		`{"t":"2026-10-11T11:00:00Z","id":"a","seven_day":{"percent":20}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newTestRecorder(t, path, nil, &now)
	r.load()

	samples, _ := r.Snapshot(time.Time{})
	if len(samples) != 1 || samples[0].SevenDay.Percent != 20 {
		t.Fatalf("samples = %+v", samples)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(data), "\n"); lines != 1 {
		t.Fatalf("file has %d lines after rewrite, want 1:\n%s", lines, data)
	}
}

func TestSampleOneUnknownCredential(t *testing.T) {
	now := time.Now()
	r := newTestRecorder(t, "", []*coreauth.Auth{{ID: "codex", Provider: "codex"}}, &now)
	if r.SampleOne(context.Background(), "codex") {
		t.Fatal("SampleOne sampled a non-Claude credential")
	}
}
