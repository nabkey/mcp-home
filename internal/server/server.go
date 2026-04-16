// Package server provides shared MCP server setup.
package server

import (
	"log/slog"

	"github.com/nabkey/mcp-home/internal/config"
	"github.com/nabkey/mcp-home/internal/frigate"
	"github.com/nabkey/mcp-home/internal/hass"
	"github.com/nabkey/mcp-home/internal/lists"
	"github.com/nabkey/mcp-home/internal/media"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// New creates a configured MCP server with tools registered based on cfg.
func New(cfg config.CLI, logger *slog.Logger) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "homeassistant",
		Version: "0.1.0",
	}, &mcp.ServerOptions{
		Instructions: "Home Assistant MCP server. Provides tools to query and control smart home devices, manage automations, view event history, manage to-do lists, search/add media via Sonarr/Radarr, and view Frigate NVR cameras and detection events.",
		Logger:       logger,
	})

	if cfg.Hass.Enabled() {
		hassTools, err := hass.NewTools(cfg.Hass.URL, cfg.Hass.Token)
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

	return server
}
