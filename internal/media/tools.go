package media

import (
	"context"
	"fmt"

	"github.com/nabkey/mcp-home/internal/mcputil"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tools holds media management tools for Sonarr and Radarr.
type Tools struct {
	sonarr *SonarrClient
	radarr *RadarrClient
}

// NewTools creates a new Tools instance. Either or both services can be configured.
func NewTools(sonarrURL, sonarrKey, radarrURL, radarrKey string) (*Tools, error) {
	t := &Tools{}

	sonarr, err := NewSonarrClient(sonarrURL, sonarrKey)
	if err == nil {
		t.sonarr = sonarr
	}

	radarr, err := NewRadarrClient(radarrURL, radarrKey)
	if err == nil {
		t.radarr = radarr
	}

	if t.sonarr == nil && t.radarr == nil {
		return nil, fmt.Errorf("neither sonarr nor radarr configured")
	}

	return t, nil
}

// Register adds all available media tools to the given MCP server.
// Tools for unconfigured services are skipped.
func (t *Tools) Register(server *mcp.Server) {
	if t.radarr != nil {
		t.registerSearchMovies(server)
		t.registerAddMovie(server)
	}
	if t.sonarr != nil {
		t.registerSearchSeries(server)
		t.registerAddSeries(server)
	}
	t.registerGetDownloadQueue(server)
}

// --- search_movies ---

type searchMoviesArgs struct {
	Term string `json:"term" jsonschema:"Search term for the movie name"`
}

func (t *Tools) registerSearchMovies(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_movies",
		Description: "Search for movies by name using Radarr. Returns matching movies with TMDB IDs that can be used to add them.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args searchMoviesArgs) (*mcp.CallToolResult, any, error) {
		if args.Term == "" {
			return mcputil.TextResult("Error: search term is required"), nil, nil
		}

		results, err := t.radarr.SearchMovies(ctx, args.Term)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		return mcputil.JSONResult(map[string]any{
			"results": results,
			"count":   len(results),
		})
	})
}

// --- add_movie ---

type addMovieArgs struct {
	TmdbID              int    `json:"tmdb_id" jsonschema:"The TMDB ID of the movie (from search results)"`
	Title               string `json:"title" jsonschema:"The title of the movie"`
	QualityProfileID    int    `json:"quality_profile_id,omitempty" jsonschema:"Quality profile ID (omit for default)"`
	RootFolderPath      string `json:"root_folder_path,omitempty" jsonschema:"Root folder path (omit for default)"`
	MinimumAvailability string `json:"minimum_availability,omitempty" jsonschema:"When available: announced inCinemas released (default: released)"`
	SearchNow           bool   `json:"search_now" jsonschema:"Whether to search for the movie immediately after adding"`
}

func (t *Tools) registerAddMovie(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "add_movie",
		Description: "Add a movie to Radarr for downloading. Use search_movies first to get the TMDB ID.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args addMovieArgs) (*mcp.CallToolResult, any, error) {
		if args.TmdbID == 0 {
			return mcputil.TextResult("Error: tmdb_id is required"), nil, nil
		}
		if args.Title == "" {
			return mcputil.TextResult("Error: title is required"), nil, nil
		}

		rootFolder, qualityProfileID, err := resolveDefaults(ctx, t.radarr.Client, args.RootFolderPath, args.QualityProfileID)
		if err != nil {
			return mcputil.Errorf("resolving defaults: %v", err), nil, nil
		}

		minAvail := args.MinimumAvailability
		if minAvail == "" {
			minAvail = "released"
		}

		result, err := t.radarr.AddMovie(ctx, AddMovieRequest{
			TmdbID:              args.TmdbID,
			Title:               args.Title,
			QualityProfileID:    qualityProfileID,
			RootFolderPath:      rootFolder,
			Monitored:           true,
			MinimumAvailability: minAvail,
			AddOptions: AddMovieOptions{
				SearchForMovie: args.SearchNow,
			},
		})
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		return mcputil.JSONResult(map[string]any{
			"success": true,
			"movie":   result,
		})
	})
}

// --- search_series ---

type searchSeriesArgs struct {
	Term string `json:"term" jsonschema:"Search term for the TV series name"`
}

func (t *Tools) registerSearchSeries(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_series",
		Description: "Search for TV series by name using Sonarr. Returns matching series with TVDB IDs that can be used to add them.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args searchSeriesArgs) (*mcp.CallToolResult, any, error) {
		if args.Term == "" {
			return mcputil.TextResult("Error: search term is required"), nil, nil
		}

		results, err := t.sonarr.SearchSeries(ctx, args.Term)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		return mcputil.JSONResult(map[string]any{
			"results": results,
			"count":   len(results),
		})
	})
}

// --- add_series ---

type addSeriesArgs struct {
	TvdbID           int    `json:"tvdb_id" jsonschema:"The TVDB ID of the series (from search results)"`
	Title            string `json:"title" jsonschema:"The title of the series"`
	Monitor          string `json:"monitor" jsonschema:"Which episodes to monitor: all latestSeason firstSeason future missing existing pilot none"`
	QualityProfileID int    `json:"quality_profile_id,omitempty" jsonschema:"Quality profile ID (omit for default)"`
	RootFolderPath   string `json:"root_folder_path,omitempty" jsonschema:"Root folder path (omit for default)"`
	SearchNow        bool   `json:"search_now" jsonschema:"Whether to search for episodes immediately after adding"`
}

var validMonitorOptions = map[string]bool{
	"all": true, "future": true, "missing": true, "existing": true,
	"pilot": true, "firstSeason": true, "latestSeason": true, "none": true,
}

func (t *Tools) registerAddSeries(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "add_series",
		Description: "Add a TV series to Sonarr for downloading. Use search_series first to get the TVDB ID.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args addSeriesArgs) (*mcp.CallToolResult, any, error) {
		if args.TvdbID == 0 {
			return mcputil.TextResult("Error: tvdb_id is required"), nil, nil
		}
		if args.Title == "" {
			return mcputil.TextResult("Error: title is required"), nil, nil
		}
		if args.Monitor == "" {
			return mcputil.TextResult("Error: monitor is required (all, latestSeason, firstSeason, future, missing, existing, pilot, none)"), nil, nil
		}
		if !validMonitorOptions[args.Monitor] {
			return mcputil.TextResult(fmt.Sprintf("Error: invalid monitor option '%s'", args.Monitor)), nil, nil
		}

		rootFolder, qualityProfileID, err := resolveDefaults(ctx, t.sonarr.Client, args.RootFolderPath, args.QualityProfileID)
		if err != nil {
			return mcputil.Errorf("resolving defaults: %v", err), nil, nil
		}

		result, err := t.sonarr.AddSeries(ctx, AddSeriesRequest{
			TvdbID:           args.TvdbID,
			Title:            args.Title,
			QualityProfileID: qualityProfileID,
			RootFolderPath:   rootFolder,
			Monitored:        true,
			SeasonFolder:     true,
			AddOptions: AddSeriesOptions{
				Monitor:                  args.Monitor,
				SearchForMissingEpisodes: args.SearchNow,
			},
		})
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		return mcputil.JSONResult(map[string]any{
			"success": true,
			"series":  result,
		})
	})
}

// --- get_download_queue ---

type getDownloadQueueArgs struct {
	Service string `json:"service" jsonschema:"Which service: sonarr radarr or both (default)"`
	Limit   int    `json:"limit,omitempty" jsonschema:"Maximum items to return (default: 20)"`
}

type queueSummary struct {
	Service  string  `json:"service"`
	Title    string  `json:"title"`
	Status   string  `json:"status"`
	State    string  `json:"state,omitempty"`
	Progress float64 `json:"progress_percent"`
	ETA      string  `json:"eta,omitempty"`
	Size     string  `json:"size"`
	Error    string  `json:"error,omitempty"`
}

func (t *Tools) registerGetDownloadQueue(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_download_queue",
		Description: "Check the download queue status for Sonarr (TV) and/or Radarr (movies). Shows progress, status, and any errors.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getDownloadQueueArgs) (*mcp.CallToolResult, any, error) {
		limit := args.Limit
		if limit <= 0 {
			limit = 20
		}

		service := args.Service
		if service == "" {
			service = "both"
		}

		var items []queueSummary
		var totalSonarr, totalRadarr int

		if (service == "sonarr" || service == "both") && t.sonarr != nil {
			queue, err := t.sonarr.GetQueue(ctx, limit)
			if err != nil {
				if service == "sonarr" {
					return mcputil.Errorf("sonarr: %v", err), nil, nil
				}
			} else {
				totalSonarr = queue.TotalRecords
				items = append(items, buildQueueSummaries("sonarr", queue.Records)...)
			}
		}

		if (service == "radarr" || service == "both") && t.radarr != nil {
			queue, err := t.radarr.GetQueue(ctx, limit)
			if err != nil {
				if service == "radarr" {
					return mcputil.Errorf("radarr: %v", err), nil, nil
				}
			} else {
				totalRadarr = queue.TotalRecords
				items = append(items, buildQueueSummaries("radarr", queue.Records)...)
			}
		}

		return mcputil.JSONResult(map[string]any{
			"items":        items,
			"total_sonarr": totalSonarr,
			"total_radarr": totalRadarr,
		})
	})
}

// --- helpers ---

func resolveDefaults(ctx context.Context, c *Client, rootFolder string, qualityProfileID int) (string, int, error) {
	if rootFolder == "" {
		folders, err := c.GetRootFolders(ctx)
		if err != nil {
			return "", 0, fmt.Errorf("failed to get root folders: %w", err)
		}
		if len(folders) > 0 {
			if path, ok := folders[0]["path"].(string); ok {
				rootFolder = path
			}
		}
		if rootFolder == "" {
			return "", 0, fmt.Errorf("no root folder configured")
		}
	}

	if qualityProfileID == 0 {
		profiles, err := c.GetQualityProfiles(ctx)
		if err != nil {
			return "", 0, fmt.Errorf("failed to get quality profiles: %w", err)
		}
		if len(profiles) > 0 {
			if id, ok := profiles[0]["id"].(float64); ok {
				qualityProfileID = int(id)
			}
		}
		if qualityProfileID == 0 {
			return "", 0, fmt.Errorf("no quality profile configured")
		}
	}

	return rootFolder, qualityProfileID, nil
}

func buildQueueSummaries(service string, records []QueueItem) []queueSummary {
	var items []queueSummary
	for _, item := range records {
		errMsg := item.ErrorMessage
		if len(item.StatusMessages) > 0 {
			if msgs, ok := item.StatusMessages[0].(map[string]any); ok {
				if msg, ok := msgs["messages"].([]any); ok && len(msg) > 0 {
					errMsg = fmt.Sprintf("%v", msg[0])
				}
			}
		}
		items = append(items, queueSummary{
			Service:  service,
			Title:    item.Title,
			Status:   item.Status,
			State:    item.TrackedDownloadState,
			Progress: item.Progress,
			ETA:      item.EstimatedCompletionTime,
			Size:     formatBytes(item.Size),
			Error:    errMsg,
		})
	}
	return items
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
