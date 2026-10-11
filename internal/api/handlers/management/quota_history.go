package management

import (
	"context"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const quotaHistoryFileName = "quota-history.jsonl"

type quotaHistoryCredential struct {
	ID         string              `json:"id"`
	AuthIndex  string              `json:"auth_index"`
	Name       string              `json:"name"`
	Email      string              `json:"email,omitempty"`
	Disabled   bool                `json:"disabled"`
	CooldownTo time.Time           `json:"cooldown_until,omitzero"`
	Status     quotahistory.Status `json:"status"`
}

func quotaHistoryPath(configFilePath string) string {
	if strings.TrimSpace(configFilePath) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configFilePath), quotaHistoryFileName)
}

func (h *Handler) listAuths() []*coreauth.Auth {
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		return nil
	}
	return manager.List()
}

// StartQuotaHistory starts background quota sampling until ctx is done.
func (h *Handler) StartQuotaHistory(ctx context.Context) {
	if h == nil {
		return
	}
	h.quotaHistory.Start(ctx)
}

// GetQuotaHistory returns Claude credentials with their latest quota windows and the recorded
// history. Query: days (1-14, default 14).
func (h *Handler) GetQuotaHistory(c *gin.Context) {
	days := 14
	if raw := strings.TrimSpace(c.Query("days")); raw != "" {
		if parsed, errParse := strconv.Atoi(raw); errParse == nil && parsed >= 1 && parsed <= 14 {
			days = parsed
		}
	}
	c.JSON(http.StatusOK, h.quotaHistoryPayload(time.Duration(days)*24*time.Hour))
}

// RefreshQuotaHistory samples one credential (body auth_index) or all Claude credentials now.
func (h *Handler) RefreshQuotaHistory(c *gin.Context) {
	var req credentialQuotaRequest
	_ = c.ShouldBindJSON(&req)
	if authIndex := req.resolveAuthIndex(); authIndex != "" {
		auth := h.authByIndex(authIndex)
		if auth == nil || !h.quotaHistory.SampleOne(c.Request.Context(), auth.ID) {
			c.JSON(http.StatusNotFound, gin.H{"error": "claude credential not found"})
			return
		}
	} else {
		h.quotaHistory.SampleAll(c.Request.Context())
	}
	c.JSON(http.StatusOK, h.quotaHistoryPayload(quotahistory.Retention))
}

func (h *Handler) quotaHistoryPayload(window time.Duration) gin.H {
	now := time.Now()
	samples, latest := h.quotaHistory.Snapshot(now.Add(-window))

	credentials := make([]quotaHistoryCredential, 0)
	for _, auth := range h.listAuths() {
		if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") {
			continue
		}
		auth.EnsureIndex()
		credential := quotaHistoryCredential{
			ID:        auth.ID,
			AuthIndex: auth.Index,
			Name:      quotaCredentialName(auth),
			Email:     authEmail(auth),
			Disabled:  auth.Disabled,
			Status:    latest[auth.ID],
		}
		if auth.Unavailable && auth.NextRetryAfter.After(now) {
			credential.CooldownTo = auth.NextRetryAfter
		}
		if auth.Quota.Exceeded && auth.Quota.NextRecoverAt.After(credential.CooldownTo) {
			credential.CooldownTo = auth.Quota.NextRecoverAt
		}
		credentials = append(credentials, credential)
	}
	sort.Slice(credentials, func(i, j int) bool { return credentials[i].Name < credentials[j].Name })

	return gin.H{
		"now":              now,
		"interval_seconds": int(quotahistory.Interval / time.Second),
		"credentials":      credentials,
		"samples":          samples,
	}
}

func quotaCredentialName(auth *coreauth.Auth) string {
	if name := strings.TrimSpace(auth.FileName); name != "" {
		return strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	}
	if label := strings.TrimSpace(auth.Label); label != "" {
		return label
	}
	return auth.ID
}
