package api

import (
	_ "embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed quota_dashboard.html
var quotaDashboardHTML []byte

// serveQuotaDashboard serves the Claude quota dashboard page. Data comes from the management
// API, so the page itself holds nothing secret; it follows the control panel's availability.
func (s *Server) serveQuotaDashboard(c *gin.Context) {
	cfg := s.getConfig()
	if cfg == nil || cfg.Home.Enabled || cfg.RemoteManagement.DisableControlPanel {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", quotaDashboardHTML)
}
