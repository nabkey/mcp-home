// Package server provides shared MCP server setup.
package server

import (
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nabkey/mcp-home/internal/config"
	"github.com/nabkey/mcp-home/internal/esphome"
	"github.com/nabkey/mcp-home/internal/frigate"
	"github.com/nabkey/mcp-home/internal/hass"
	"github.com/nabkey/mcp-home/internal/lists"
	"github.com/nabkey/mcp-home/internal/media"
)

// New creates a configured MCP server with tools registered based on cfg.
// version is the build version injected into the binary (e.g. "1.4.0" or "dev").
func New(cfg config.CLI, version string, logger *slog.Logger) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "mcp-home",
		Version: version,
	}, &mcp.ServerOptions{
		Instructions: "Home Assistant MCP server. Provides tools to query and control smart home devices, manage automations and scripts, view event history, manage to-do lists, search/add media via Sonarr/Radarr, view Frigate NVR cameras and detection events, and manage ESPHome devices (read/write configs, validate, compile, OTA upload, logs).",
		Logger:       logger,
	})

	// Audit every tool call with the authenticated user.
	server.AddReceivingMiddleware(auditMiddleware(logger))

	if cfg.Hass.Enabled() {
		hassTools, err := hass.NewTools(cfg.Hass.URL, cfg.Hass.Token, cfg.Hass.DenyServices)
		if err != nil {
			logger.Warn("Home Assistant tools failed", "error", err)
		} else {
			hassTools.Register(server)
			logger.Info("Home Assistant tools registered")

			listTools, err := lists.NewTools(hassTools.Client())
			if err != nil {
				logger.Warn("List tools failed", "error", err)
			} else {
				listTools.Register(server)
				logger.Info("List management tools registered")
			}
		}
	} else {
		logger.Info("Home Assistant not configured, skipping")
	}

	// Media tools handle partial config (Sonarr only, Radarr only, or both).
	if cfg.Sonarr.Enabled() || cfg.Radarr.Enabled() {
		mediaTools, err := media.NewTools(
			cfg.Sonarr.URL, cfg.Sonarr.APIKey,
			cfg.Radarr.URL, cfg.Radarr.APIKey,
		)
		if err != nil {
			logger.Warn("Media tools failed", "error", err)
		} else {
			mediaTools.Register(server)
			logger.Info("Media tools registered")
		}
	} else {
		logger.Info("Media (Sonarr/Radarr) not configured, skipping")
	}

	if cfg.Frigate.Enabled() {
		frigateTools, err := frigate.NewTools(cfg.Frigate.URL)
		if err != nil {
			logger.Warn("Frigate tools failed", "error", err)
		} else {
			frigateTools.Register(server)
			logger.Info("Frigate tools registered")
		}
	} else {
		logger.Info("Frigate not configured, skipping")
	}

	if cfg.ESPHome.Enabled() {
		esphomeTools, err := esphome.NewTools(cfg.ESPHome.URL, cfg.ESPHome.Password)
		if err != nil {
			logger.Warn("ESPHome tools failed", "error", err)
		} else {
			esphomeTools.Register(server)
			logger.Info("ESPHome tools registered")
		}
	} else {
		logger.Info("ESPHome not configured, skipping")
	}

	return server
}
