package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"squire/internal/buildinfo"
)

const (
	defaultReleaseOwner = "reidgoodbar"
	defaultReleaseRepo  = "squire"
)

type updateResponse struct {
	Status           string `json:"status"`
	CurrentVersion   string `json:"current_version"`
	InstalledVersion string `json:"installed_version"`
	InstalledPath    string `json:"installed_path"`
	SourceURL        string `json:"source_url"`
}

func runUpdate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	version := fs.String("version", "latest", "Release tag to install, or latest")
	installDir := fs.String("install-dir", "", "Directory to write the updated squire binary into")
	jsonOut := fs.Bool("json", false, "Print machine-readable JSON output")
	fs.Usage = func() { printUpdateHelp(fs.Output()) }
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	resp, err := performUpdate(ctx, *version, *installDir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitRemoteError
	}

	if *jsonOut {
		_ = json.NewEncoder(stdout).Encode(resp)
		return exitOK
	}

	fmt.Fprintf(stdout, "updated squire from %s to %s at %s\n", resp.CurrentVersion, resp.InstalledVersion, resp.InstalledPath)
	return exitOK
}

func performUpdate(ctx context.Context, version, installDir string) (updateResponse, error) {
	asset, err := releaseAssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return updateResponse{}, err
	}
	targetPath, err := targetInstallPath(installDir)
	if err != nil {
		return updateResponse{}, err
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return updateResponse{}, err
	}

	downloadURL := releaseDownloadURL(version, asset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return updateResponse{}, err
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	httpResp, err := client.Do(req)
	if err != nil {
		return updateResponse{}, err
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		return updateResponse{}, fmt.Errorf("download failed: %s", httpResp.Status)
	}

	archiveFile, err := os.CreateTemp("", "squire-update-*.tar.gz")
	if err != nil {
		return updateResponse{}, err
	}
	archivePath := archiveFile.Name()
	defer os.Remove(archivePath)
	if _, err := io.Copy(archiveFile, httpResp.Body); err != nil {
		_ = archiveFile.Close()
		return updateResponse{}, err
	}
	if err := archiveFile.Close(); err != nil {
		return updateResponse{}, err
	}

	extractedBinary, err := extractArchiveBinary(archivePath)
	if err != nil {
		return updateResponse{}, err
	}
	defer os.RemoveAll(filepath.Dir(extractedBinary))

	tempFile, err := os.CreateTemp(filepath.Dir(targetPath), "squire-update-*")
	if err != nil {
		return updateResponse{}, err
	}
	tempPath := tempFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()

	if err := copyFileContents(tempFile, extractedBinary); err != nil {
		_ = tempFile.Close()
		return updateResponse{}, err
	}
	if err := tempFile.Chmod(0o755); err != nil {
		_ = tempFile.Close()
		return updateResponse{}, err
	}
	if err := tempFile.Close(); err != nil {
		return updateResponse{}, err
	}
	if err := os.Rename(tempPath, targetPath); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return updateResponse{}, fmt.Errorf("failed to replace %s: %w; rerun with permissions for that directory or use --install-dir", targetPath, err)
		}
		return updateResponse{}, err
	}
	cleanup = false

	return updateResponse{
		Status:           "ok",
		CurrentVersion:   buildinfo.CurrentVersion(),
		InstalledVersion: resolvedReleaseVersion(version, httpResp.Request.URL),
		InstalledPath:    targetPath,
		SourceURL:        httpResp.Request.URL.String(),
	}, nil
}

func releaseAssetName(goos, goarch string) (string, error) {
	switch goos {
	case "linux", "darwin":
	default:
		return "", fmt.Errorf("unsupported OS for self-update: %s", goos)
	}

	var arch string
	switch goarch {
	case "amd64":
		arch = "amd64"
	case "arm64":
		arch = "arm64"
	default:
		return "", fmt.Errorf("unsupported architecture for self-update: %s", goarch)
	}
	return fmt.Sprintf("squire_%s_%s.tar.gz", goos, arch), nil
}

func targetInstallPath(installDir string) (string, error) {
	if installDir != "" {
		return filepath.Join(installDir, "squire"), nil
	}
	execPath, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(execPath), "squire"), nil
}

func releaseDownloadURL(version, asset string) string {
	if base := strings.TrimRight(os.Getenv("SQUIRE_UPDATE_BASE_URL"), "/"); base != "" {
		return base + "/" + url.PathEscape(version) + "/" + url.PathEscape(asset)
	}
	owner := os.Getenv("SQUIRE_INSTALL_REPO_OWNER")
	if owner == "" {
		owner = defaultReleaseOwner
	}
	repo := os.Getenv("SQUIRE_INSTALL_REPO_NAME")
	if repo == "" {
		repo = defaultReleaseRepo
	}
	if version == "latest" {
		return fmt.Sprintf("https://github.com/%s/%s/releases/latest/download/%s", owner, repo, asset)
	}
	return fmt.Sprintf("https://github.com/%s/%s/releases/download/%s/%s", owner, repo, version, asset)
}

func extractArchiveBinary(archivePath string) (string, error) {
	extractDir, err := os.MkdirTemp("", "squire-update-extract-*")
	if err != nil {
		return "", err
	}
	cmd := exec.Command("tar", "-C", extractDir, "-xzf", archivePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(extractDir)
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("failed to unpack release archive: %s", message)
	}
	binaryPath := filepath.Join(extractDir, "squire")
	if _, err := os.Stat(binaryPath); err != nil {
		_ = os.RemoveAll(extractDir)
		return "", errors.New("release archive did not contain a squire binary")
	}
	return binaryPath, nil
}

func copyFileContents(dst *os.File, srcPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	_, err = io.Copy(dst, src)
	return err
}

func resolvedReleaseVersion(requested string, finalURL *url.URL) string {
	if requested != "" && requested != "latest" {
		return requested
	}
	if finalURL == nil {
		return "latest"
	}
	parts := strings.Split(strings.Trim(finalURL.Path, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "download" {
			return parts[i+1]
		}
	}
	return "latest"
}
