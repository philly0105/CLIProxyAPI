// Package quotahistory periodically records Claude credential quota usage so the quota
// dashboard can chart burn over time and project when limits run out.
package quotahistory

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// Interval is how often every Claude credential is sampled.
	Interval = 10 * time.Minute
	// Retention is how long samples are kept.
	Retention = 14 * 24 * time.Hour

	fetchTimeout = 20 * time.Second
)

// Sample is one recorded usage reading for one credential.
type Sample struct {
	At time.Time `json:"t"`
	ID string    `json:"id"`
	coreauth.ClaudeUsage
}

// Status is the latest sampling outcome for one credential.
type Status struct {
	Usage     coreauth.ClaudeUsage `json:"usage"`
	FetchedAt time.Time            `json:"fetched_at,omitzero"`
	Source    string               `json:"source,omitempty"`
	Error     string               `json:"error,omitempty"`
}

// Recorder samples Claude credentials and keeps a bounded history, persisted as JSON lines.
type Recorder struct {
	path  string
	list  func() []*coreauth.Auth
	fetch func(ctx context.Context, auth *coreauth.Auth) (coreauth.ClaudeUsage, error)
	now   func() time.Time

	mu      sync.Mutex
	samples []Sample
	latest  map[string]Status
	running bool
}

// New returns a recorder that persists to path (in memory only when path is empty) and samples
// the credentials returned by list.
func New(path string, list func() []*coreauth.Auth) *Recorder {
	return &Recorder{
		path:   path,
		list:   list,
		fetch:  coreauth.FetchClaudeUsage,
		now:    time.Now,
		latest: make(map[string]Status),
	}
}

// Start loads persisted history and samples every Interval until ctx is done.
func (r *Recorder) Start(ctx context.Context) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.mu.Unlock()

	r.load()
	go func() {
		ticker := time.NewTicker(Interval)
		defer ticker.Stop()
		for {
			r.SampleAll(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// SampleAll samples every enabled Claude credential.
func (r *Recorder) SampleAll(ctx context.Context) {
	for _, auth := range r.claudeAuths() {
		if !auth.Disabled {
			r.sample(ctx, auth)
		}
	}
	r.prune()
}

// SampleOne samples the credential with the given ID. It reports false when no such Claude
// credential exists.
func (r *Recorder) SampleOne(ctx context.Context, id string) bool {
	for _, auth := range r.claudeAuths() {
		if auth.ID == id {
			r.sample(ctx, auth)
			return true
		}
	}
	return false
}

// Snapshot returns samples recorded since the given time and the latest status per credential.
func (r *Recorder) Snapshot(since time.Time) ([]Sample, map[string]Status) {
	r.mu.Lock()
	defer r.mu.Unlock()
	samples := make([]Sample, 0, len(r.samples))
	for _, sample := range r.samples {
		if !sample.At.Before(since) {
			samples = append(samples, sample)
		}
	}
	latest := make(map[string]Status, len(r.latest))
	for id, status := range r.latest {
		latest[id] = status
	}
	return samples, latest
}

func (r *Recorder) claudeAuths() []*coreauth.Auth {
	if r == nil || r.list == nil {
		return nil
	}
	var out []*coreauth.Auth
	for _, auth := range r.list() {
		if auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") {
			out = append(out, auth)
		}
	}
	return out
}

// sample fetches usage for one credential, falling back to observed rate-limit headers for
// windows the usage endpoint did not report.
func (r *Recorder) sample(ctx context.Context, auth *coreauth.Auth) {
	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	usage, errFetch := r.fetch(fetchCtx, auth)
	cancel()

	var status Status
	if errFetch != nil {
		status.Error = errFetch.Error()
		usage = coreauth.ClaudeUsage{}
	} else {
		status.Source = "usage"
	}
	headers := coreauth.ClaudeHeaderUsage(auth)
	usedHeaders := false
	if usage.FiveHour == nil && headers.FiveHour != nil {
		usage.FiveHour, usedHeaders = headers.FiveHour, true
	}
	if usage.SevenDay == nil && headers.SevenDay != nil {
		usage.SevenDay, usedHeaders = headers.SevenDay, true
	}
	if usedHeaders {
		status.Source = strings.TrimPrefix(status.Source+"+headers", "+")
	}

	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if usage.Empty() {
		previous := r.latest[auth.ID]
		status.Usage, status.FetchedAt = previous.Usage, previous.FetchedAt
		r.latest[auth.ID] = status
		return
	}
	status.Usage, status.FetchedAt = usage, now
	r.latest[auth.ID] = status
	sample := Sample{At: now, ID: auth.ID, ClaudeUsage: usage}
	r.samples = append(r.samples, sample)
	r.appendLine(sample)
}

// prune drops expired samples and rewrites the file once enough of it is stale.
func (r *Recorder) prune() {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := r.now().Add(-Retention)
	keep := 0
	for keep < len(r.samples) && r.samples[keep].At.Before(cutoff) {
		keep++
	}
	if keep == 0 {
		return
	}
	r.samples = append([]Sample(nil), r.samples[keep:]...)
	if keep >= 500 {
		r.rewrite()
	}
}

func (r *Recorder) load() {
	if r.path == "" {
		return
	}
	data, errRead := os.ReadFile(r.path)
	if errRead != nil {
		if !os.IsNotExist(errRead) {
			log.WithError(errRead).Warn("quota history: failed to read history file")
		}
		return
	}
	cutoff := r.now().Add(-Retention)
	var samples []Sample
	dropped := 0
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		var sample Sample
		if errUnmarshal := json.Unmarshal(scanner.Bytes(), &sample); errUnmarshal != nil || sample.ID == "" || sample.At.Before(cutoff) {
			dropped++
			continue
		}
		samples = append(samples, sample)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples = append(samples, r.samples...)
	for _, sample := range samples {
		if current, ok := r.latest[sample.ID]; !ok || sample.At.After(current.FetchedAt) {
			r.latest[sample.ID] = Status{Usage: sample.ClaudeUsage, FetchedAt: sample.At, Source: "history"}
		}
	}
	if dropped > 0 {
		r.rewrite()
	}
}

// appendLine persists one sample. Callers hold r.mu.
func (r *Recorder) appendLine(sample Sample) {
	if r.path == "" {
		return
	}
	line, errMarshal := json.Marshal(sample)
	if errMarshal != nil {
		return
	}
	file, errOpen := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if errOpen != nil {
		log.WithError(errOpen).Warn("quota history: failed to open history file")
		return
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.WithError(errClose).Warn("quota history: failed to close history file")
		}
	}()
	if _, errWrite := file.Write(append(line, '\n')); errWrite != nil {
		log.WithError(errWrite).Warn("quota history: failed to append sample")
	}
}

// rewrite replaces the history file with the in-memory samples. Callers hold r.mu.
func (r *Recorder) rewrite() {
	if r.path == "" {
		return
	}
	var buf bytes.Buffer
	for _, sample := range r.samples {
		line, errMarshal := json.Marshal(sample)
		if errMarshal != nil {
			continue
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	tmp := r.path + ".tmp"
	if errWrite := os.WriteFile(tmp, buf.Bytes(), 0o600); errWrite != nil {
		log.WithError(errWrite).Warn("quota history: failed to write history file")
		return
	}
	if errRename := os.Rename(tmp, r.path); errRename != nil {
		log.WithError(errRename).Warn("quota history: failed to replace history file")
	}
}
