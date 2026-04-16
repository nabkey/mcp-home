// Package frigate provides MCP tools for interacting with Frigate NVR.
package frigate

import (
	"context"
	"time"

	"github.com/nabkey/mcp-home/internal/mcputil"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tools holds the Frigate tools and their configuration.
type Tools struct {
	client *Client
}

// NewTools creates a new Tools instance.
// If frigateURL is empty, it reads from FRIGATE_URL environment variable.
func NewTools(frigateURL string) (*Tools, error) {
	client, err := NewClient(frigateURL)
	if err != nil {
		return nil, err
	}
	return &Tools{client: client}, nil
}

// Register adds all Frigate tools to the given MCP server.
func (t *Tools) Register(server *mcp.Server) {
	t.registerListCameras(server)
	t.registerGetCameraSnapshot(server)
	t.registerGetEvents(server)
	t.registerGetEventSnapshot(server)
}

// --- list_frigate_cameras ---

type listCamerasArgs struct{}

func (t *Tools) registerListCameras(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_frigate_cameras",
		Description: "List all available cameras in Frigate NVR.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args listCamerasArgs) (*mcp.CallToolResult, any, error) {
		config, err := t.client.GetConfig(ctx)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		cameras := make([]string, 0, len(config.Cameras))
		for name, cam := range config.Cameras {
			if cam.Enabled {
				cameras = append(cameras, name)
			}
		}

		return mcputil.JSONResult(map[string]any{
			"cameras": cameras,
			"count":   len(cameras),
		})
	})
}

// --- get_camera_snapshot ---

type getCameraSnapshotArgs struct {
	Camera string `json:"camera" jsonschema:"Name of the camera to get a snapshot from (e.g. front_door backyard)"`
}

func (t *Tools) registerGetCameraSnapshot(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_camera_snapshot",
		Description: "Get a current snapshot image from a Frigate camera as base64-encoded JPEG.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getCameraSnapshotArgs) (*mcp.CallToolResult, any, error) {
		if args.Camera == "" {
			return mcputil.TextResult("Error: camera name is required"), nil, nil
		}

		data, err := t.client.GetLatestFrame(ctx, args.Camera)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.ImageContent{
					MIMEType: "image/jpeg",
					Data:     data,
				},
			},
		}, nil, nil
	})
}

// --- get_frigate_events ---

type getEventsArgs struct {
	Camera string `json:"camera,omitempty" jsonschema:"Optional camera name to filter events"`
	Label  string `json:"label,omitempty" jsonschema:"Optional label to filter events (e.g. person car dog)"`
	Hours  int    `json:"hours,omitempty" jsonschema:"Number of hours to look back (default: 1 max: 24)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum number of events to return (default: 10 max: 50)"`
}

type eventSummary struct {
	ID        string   `json:"id"`
	Camera    string   `json:"camera"`
	Label     string   `json:"label"`
	Score     float64  `json:"score"`
	StartTime string   `json:"start_time"`
	EndTime   string   `json:"end_time,omitempty"`
	Zones     []string `json:"zones,omitempty"`
	HasSnap   bool     `json:"has_snapshot"`
}

func (t *Tools) registerGetEvents(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_frigate_events",
		Description: "Get recent detection events from Frigate NVR. Shows when people, cars, animals, etc. were detected by cameras.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getEventsArgs) (*mcp.CallToolResult, any, error) {
		hours := args.Hours
		if hours <= 0 {
			hours = 1
		}
		if hours > 24 {
			hours = 24
		}

		limit := args.Limit
		if limit <= 0 {
			limit = 10
		}
		if limit > 50 {
			limit = 50
		}

		after := time.Now().Add(-time.Duration(hours) * time.Hour)

		var cameras, labels []string
		if args.Camera != "" {
			cameras = []string{args.Camera}
		}
		if args.Label != "" {
			labels = []string{args.Label}
		}

		events, err := t.client.GetEvents(ctx, cameras, labels, limit, &after)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		summaries := make([]eventSummary, 0, len(events))
		for _, e := range events {
			s := eventSummary{
				ID:        e.ID,
				Camera:    e.Camera,
				Label:     e.Label,
				Score:     e.TopScore,
				StartTime: time.Unix(int64(e.StartTime), 0).Format(time.RFC3339),
				Zones:     e.EnteredZones,
				HasSnap:   e.HasSnapshot,
			}
			if e.EndTime != nil {
				s.EndTime = time.Unix(int64(*e.EndTime), 0).Format(time.RFC3339)
			}
			summaries = append(summaries, s)
		}

		return mcputil.JSONResult(map[string]any{
			"events": summaries,
			"count":  len(summaries),
		})
	})
}

// --- get_event_snapshot ---

type getEventSnapshotArgs struct {
	EventID string `json:"event_id" jsonschema:"The event ID to get the snapshot for"`
}

func (t *Tools) registerGetEventSnapshot(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_event_snapshot",
		Description: "Get the snapshot image for a specific Frigate detection event as base64-encoded JPEG.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getEventSnapshotArgs) (*mcp.CallToolResult, any, error) {
		if args.EventID == "" {
			return mcputil.TextResult("Error: event_id is required"), nil, nil
		}

		data, err := t.client.GetEventSnapshot(ctx, args.EventID)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.ImageContent{
					MIMEType: "image/jpeg",
					Data:     data,
				},
			},
		}, nil, nil
	})
}
