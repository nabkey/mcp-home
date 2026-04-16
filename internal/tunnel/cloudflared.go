package tunnel

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

const (
	// githubReleasesAPI is the endpoint for the latest cloudflared release.
	githubReleasesAPI = "https://api.github.com/repos/cloudflare/cloudflared/releases/latest"
	// maxDownloadSize is the maximum size we'll download (100 MB).
	maxDownloadSize = 100 * 1024 * 1024
)

// ensureCloudflared returns the path to the cloudflared binary.
// It first checks PATH, then falls back to a cached download in ~/.cache/mcp-server/.
func ensureCloudflared(ctx context.Context, logger *slog.Logger) (string, error) {
	// Check PATH first.
	if path, err := exec.LookPath("cloudflared"); err == nil {
		return path, nil
	}

	cacheDir, err := cloudflaredCacheDir()
	if err != nil {
		return "", err
	}
	binPath := filepath.Join(cacheDir, "cloudflared")

	// If we already downloaded it, reuse it.
	if _, err := os.Stat(binPath); err == nil {
		return binPath, nil
	}

	logger.Info("cloudflared not found on PATH, downloading...")

	version, err := latestVersion(ctx)
	if err != nil {
		return "", fmt.Errorf("fetch latest cloudflared version: %w", err)
	}

	downloadURL, err := buildDownloadURL(version)
	if err != nil {
		return "", err
	}

	logger.Info("downloading cloudflared", "version", version, "url", downloadURL)

	if err := downloadCloudflared(ctx, downloadURL, binPath); err != nil {
		return "", fmt.Errorf("download cloudflared: %w", err)
	}

	logger.Info("cloudflared downloaded", "path", binPath)
	return binPath, nil
}

func cloudflaredCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	dir := filepath.Join(home, ".cache", "mcp-server")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}
	return dir, nil
}

type githubRelease struct {
	TagName string `json:"tag_name"`
}

func latestVersion(ctx context.Context) (string, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", githubReleasesAPI, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&release); err != nil {
		return "", err
	}
	if release.TagName == "" {
		return "", fmt.Errorf("empty tag_name in GitHub release response")
	}
	return release.TagName, nil
}

func buildDownloadURL(version string) (string, error) {
	goos := runtime.GOOS
	goarch := runtime.GOARCH

	switch {
	case goos == "darwin" && goarch == "arm64":
		return fmt.Sprintf("https://github.com/cloudflare/cloudflared/releases/download/%s/cloudflared-darwin-arm64.tgz", version), nil
	case goos == "darwin" && goarch == "amd64":
		return fmt.Sprintf("https://github.com/cloudflare/cloudflared/releases/download/%s/cloudflared-darwin-amd64.tgz", version), nil
	case goos == "linux" && goarch == "amd64":
		return fmt.Sprintf("https://github.com/cloudflare/cloudflared/releases/download/%s/cloudflared-linux-amd64", version), nil
	case goos == "linux" && goarch == "arm64":
		return fmt.Sprintf("https://github.com/cloudflare/cloudflared/releases/download/%s/cloudflared-linux-arm64", version), nil
	default:
		return "", fmt.Errorf("unsupported platform: %s/%s", goos, goarch)
	}
}

func downloadCloudflared(ctx context.Context, downloadURL, destPath string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	body := io.LimitReader(resp.Body, maxDownloadSize)

	// Darwin releases are .tgz archives; Linux releases are bare binaries.
	if runtime.GOOS == "darwin" {
		return extractFromTgz(body, destPath)
	}
	return writeBinary(body, destPath)
}

func extractFromTgz(r io.Reader, destPath string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip open: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("cloudflared binary not found in archive")
		}
		if err != nil {
			return fmt.Errorf("tar read: %w", err)
		}
		if filepath.Base(hdr.Name) == "cloudflared" && hdr.Typeflag == tar.TypeReg {
			return writeBinary(tr, destPath)
		}
	}
}

func writeBinary(r io.Reader, destPath string) error {
	// Write to a temp file in the same directory, then rename for atomicity.
	dir := filepath.Dir(destPath)
	tmp, err := os.CreateTemp(dir, "cloudflared-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpPath) // no-op if rename succeeded
	}()

	if _, err := io.Copy(tmp, r); err != nil {
		return fmt.Errorf("write binary: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
