package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/managementasset"
)

// GetManagementPanelLatestVersion returns the latest management panel release version.
func (h *Handler) GetManagementPanelLatestVersion(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config_unavailable", "message": "configuration is unavailable"})
		return
	}

	release, err := managementasset.GetLatestRelease(
		c.Request.Context(),
		h.cfg.ProxyURL,
		h.cfg.RemoteManagement.PanelGitHubRepository,
	)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "request_failed", "message": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"latest-version": release.Version})
}

// UpdateManagementPanel downloads and installs the latest management panel asset.
func (h *Handler) UpdateManagementPanel(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config_unavailable", "message": "configuration is unavailable"})
		return
	}
	if h.cfg.RemoteManagement.DisableControlPanel {
		c.JSON(http.StatusConflict, gin.H{"error": "control_panel_disabled", "message": "control panel is disabled"})
		return
	}

	staticDir := managementasset.StaticDir(h.configFilePath)
	if strings.TrimSpace(staticDir) == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "static_dir_unavailable", "message": "management static directory is unavailable"})
		return
	}

	hash, err := managementasset.UpdateLatestManagementHTML(
		c.Request.Context(),
		staticDir,
		h.cfg.ProxyURL,
		h.cfg.RemoteManagement.PanelGitHubRepository,
	)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "update_failed", "message": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"updated": true, "hash": hash})
}
