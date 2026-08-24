package version

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	repoOwner = "plumpslabs"
	repoName  = "bro-code"
)

// VersionCache holds the cached latest release info.
type VersionCache struct {
	LatestTag   string    `json:"latest_tag"`
	CheckedAt   time.Time `json:"checked_at"`
	HasUpdate   bool      `json:"has_update"`
	DownloadURL string    `json:"download_url"`
}

// cacheFilePath returns ~/.config/brocode/version_cache.json
func cacheFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "brocode", "version_cache.json")
}

// CheckLatestVersion checks GitHub Releases for the latest version with 12h local caching.
func CheckLatestVersion(ctx context.Context, forceRefresh bool) (string, bool, error) {
	cPath := cacheFilePath()
	if !forceRefresh {
		if data, err := os.ReadFile(cPath); err == nil {
			var cache VersionCache
			if json.Unmarshal(data, &cache) == nil {
			if time.Since(cache.CheckedAt) < 12*time.Hour {
				// Re-compare against current binary version so a stale "hasUpdate"
				// from before an upgrade doesn't keep showing the notification.
				return cache.LatestTag, isNewerVersion(cache.LatestTag, Version), nil
			}
			}
		}
	}

	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", repoOwner, repoName), nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("User-Agent", "BroCode-Updater")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("github api returned status %d", resp.StatusCode)
	}

	var rel struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", false, err
	}

	latestTag := strings.TrimSpace(rel.TagName)
	hasUpdate := isNewerVersion(latestTag, Version)

	// Save to cache
	cache := VersionCache{
		LatestTag:   latestTag,
		CheckedAt:   time.Now(),
		HasUpdate:   hasUpdate,
		DownloadURL: rel.HTMLURL,
	}
	if cData, err := json.Marshal(cache); err == nil {
		_ = os.MkdirAll(filepath.Dir(cPath), 0o755)
		_ = os.WriteFile(cPath, cData, 0o644)
	}

	return latestTag, hasUpdate, nil
}

// isNewerVersion compares semver strings (e.g. "v0.1.35" > "v0.1.34").
func isNewerVersion(latest, current string) bool {
	lParts := parseSemver(latest)
	cParts := parseSemver(current)
	for i := 0; i < 3; i++ {
		if lParts[i] > cParts[i] {
			return true
		}
		if lParts[i] < cParts[i] {
			return false
		}
	}
	return false
}

func parseSemver(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	parts := strings.Split(v, ".")
	var res [3]int
	for i := 0; i < len(parts) && i < 3; i++ {
		// Strip prerelease or metadata if any
		clean := parts[i]
		if idx := strings.IndexAny(clean, "-+"); idx != -1 {
			clean = clean[:idx]
		}
		res[i], _ = strconv.Atoi(clean)
	}
	return res
}

// SelfUpdate downloads and installs the latest binary for the current OS/Arch.
func SelfUpdate(ctx context.Context, targetTag string) (string, error) {
	if targetTag == "" {
		latest, _, err := CheckLatestVersion(ctx, true)
		if err != nil {
			return "", fmt.Errorf("failed to determine latest version: %w", err)
		}
		targetTag = latest
	}

	ver := strings.TrimPrefix(targetTag, "v")
	osName := runtime.GOOS
	archName := runtime.GOARCH

	ext := "tar.gz"
	if osName == "windows" {
		ext = "zip"
	}

	archiveName := fmt.Sprintf("brocode_%s_%s_%s.%s", ver, osName, archName, ext)
	downloadURL := fmt.Sprintf("https://github.com/%s/%s/releases/download/%s/%s", repoOwner, repoName, targetTag, archiveName)

	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "BroCode-Updater")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed with status %d from %s", resp.StatusCode, downloadURL)
	}

	archiveData, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read binary archive: %w", err)
	}

	var newBinary []byte
	if ext == "zip" {
		newBinary, err = extractFromZip(archiveData, "brocode.exe")
	} else {
		newBinary, err = extractFromTarGz(archiveData, "brocode")
	}
	if err != nil {
		return "", fmt.Errorf("failed to extract executable: %w", err)
	}

	execPath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("failed to locate running executable: %w", err)
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return "", fmt.Errorf("failed to resolve binary path: %w", err)
	}

	// Safe atomic binary replacement
	oldPath := execPath + ".old"
	_ = os.Remove(oldPath)

	if err := os.Rename(execPath, oldPath); err != nil {
		return "", fmt.Errorf("failed to stage current binary for replacement: %w", err)
	}

	if err := os.WriteFile(execPath, newBinary, 0o755); err != nil {
		// Rollback if write failed
		_ = os.Rename(oldPath, execPath)
		return "", fmt.Errorf("failed to write updated binary: %w", err)
	}

	// Cleanup old binary (best-effort)
	_ = os.Remove(oldPath)

	return fmt.Sprintf("✅ BroCode successfully upgraded to %s (%s)", targetTag, execPath), nil
}

func extractFromZip(data []byte, targetName string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if strings.EqualFold(filepath.Base(f.Name), targetName) {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("%s not found in zip archive", targetName)
}

func extractFromTarGz(data []byte, targetName string) ([]byte, error) {
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(filepath.Base(hdr.Name), targetName) {
			return io.ReadAll(tr)
		}
	}
	return nil, fmt.Errorf("%s not found in tar.gz archive", targetName)
}
