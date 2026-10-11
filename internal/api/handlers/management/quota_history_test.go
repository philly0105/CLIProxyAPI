package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestGetQuotaHistoryListsClaudeCredentials(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	manager := coreauth.NewManager(nil, nil, nil)
	cooldown := time.Now().Add(2 * time.Hour)
	for _, auth := range []*coreauth.Auth{
		{ID: "c1", FileName: "claude-one@example.com.json", Provider: "claude", Metadata: map[string]any{"email": "one@example.com"}, Unavailable: true, NextRetryAfter: cooldown},
		{ID: "x1", FileName: "codex-x.json", Provider: "codex"},
	} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register: %v", errRegister)
		}
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/quota/history?days=3", nil)
	h.GetQuotaHistory(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		IntervalSeconds int                      `json:"interval_seconds"`
		Credentials     []quotaHistoryCredential `json:"credentials"`
		Samples         []json.RawMessage        `json:"samples"`
	}
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	if len(payload.Credentials) != 1 {
		t.Fatalf("credentials = %+v, want only the Claude one", payload.Credentials)
	}
	got := payload.Credentials[0]
	if got.Name != "claude-one@example.com" || got.Email != "one@example.com" || got.AuthIndex == "" || !got.CooldownTo.Equal(cooldown) {
		t.Fatalf("credential = %+v", got)
	}
	if payload.IntervalSeconds != 600 || payload.Samples == nil {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestRefreshQuotaHistoryUnknownIndex(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, coreauth.NewManager(nil, nil, nil))

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v8/management/quota/history/refresh", strings.NewReader(`{"auth_index":"missing"}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.RefreshQuotaHistory(ctx)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestQuotaHistoryPath(t *testing.T) {
	if quotaHistoryPath("") != "" {
		t.Fatal("empty config path should keep history in memory")
	}
	want := filepath.Join("dir", quotaHistoryFileName)
	if got := quotaHistoryPath(filepath.Join("dir", "config.yaml")); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}
