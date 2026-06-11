package piupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	defaultLatestURL   = "https://registry.npmjs.org/@earendil-works/pi-coding-agent/latest"
	defaultReleaseBase = "https://github.com/earendil-works/pi/releases/download"
)

type Options struct {
	HomeDir     string
	Version     string
	HTTPClient  *http.Client
	Stdout      io.Writer
	LatestURL   string
	ReleaseBase string
}

type Result struct {
	Version string
	Path    string
}

type latestPackage struct {
	Version string `json:"version"`
}

func Update(ctx context.Context, opts Options) (Result, error) {
	if opts.HomeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Result{}, fmt.Errorf("home dir: %w", err)
		}
		opts.HomeDir = home
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if opts.LatestURL == "" {
		opts.LatestURL = defaultLatestURL
	}
	if opts.ReleaseBase == "" {
		opts.ReleaseBase = defaultReleaseBase
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}

	version := strings.TrimSpace(opts.Version)
	if version == "" {
		var err error
		version, err = latestVersion(ctx, opts.HTTPClient, opts.LatestURL)
		if err != nil {
			return Result{}, err
		}
	}
	tag := "v" + strings.TrimPrefix(version, "v")
	version = strings.TrimPrefix(tag, "v")

	assetArch, err := piAssetArch()
	if err != nil {
		return Result{}, err
	}
	assetURL := strings.TrimRight(opts.ReleaseBase, "/") + "/" + tag + "/pi-linux-" + assetArch + ".tar.gz"

	localDir := filepath.Join(opts.HomeDir, ".local")
	targetDir := filepath.Join(localDir, "pi")
	binDir := filepath.Join(localDir, "bin")
	binPath := filepath.Join(targetDir, "pi")
	linkPath := filepath.Join(binDir, "pi")

	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("create local dir: %w", err)
	}
	tmpParent, err := os.MkdirTemp(localDir, ".pi-update-")
	if err != nil {
		return Result{}, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpParent)

	if err := downloadAndExtract(ctx, opts.HTTPClient, assetURL, tmpParent); err != nil {
		return Result{}, err
	}
	newDir := filepath.Join(tmpParent, "pi")
	newBin := filepath.Join(newDir, "pi")
	if err := verifyPiVersion(ctx, newBin, version); err != nil {
		return Result{}, err
	}
	if err := replaceDir(targetDir, newDir); err != nil {
		return Result{}, err
	}
	if err := ensureUserBinLink(binDir, linkPath, binPath); err != nil {
		return Result{}, err
	}

	fmt.Fprintf(opts.Stdout, "pi: updated to %s\n", version)
	return Result{Version: version, Path: binPath}, nil
}

func latestVersion(ctx context.Context, client *http.Client, latestURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return "", fmt.Errorf("latest version request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch latest version: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("fetch latest version: HTTP %d", resp.StatusCode)
	}
	var latest latestPackage
	if err := json.NewDecoder(resp.Body).Decode(&latest); err != nil {
		return "", fmt.Errorf("decode latest version: %w", err)
	}
	version := strings.TrimSpace(latest.Version)
	if version == "" {
		return "", errors.New("latest version response missing version")
	}
	return version, nil
}

func piAssetArch() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "x64", nil
	case "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported architecture: %s", runtime.GOARCH)
	}
}

func downloadAndExtract(ctx context.Context, client *http.Client, assetURL, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return fmt.Errorf("download request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download pi: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("download pi: HTTP %d", resp.StatusCode)
	}
	gzr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("read pi archive: %w", err)
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read pi archive: %w", err)
		}
		if err := extractTarEntry(dest, header, tr); err != nil {
			return err
		}
	}
	return nil
}

func extractTarEntry(dest string, header *tar.Header, r io.Reader) error {
	name := filepath.Clean(header.Name)
	if filepath.IsAbs(name) || name == "." || strings.HasPrefix(name, ".."+string(filepath.Separator)) || name == ".." {
		return fmt.Errorf("unsafe path in pi archive: %q", header.Name)
	}
	path := filepath.Join(dest, name)
	if !strings.HasPrefix(path, dest+string(filepath.Separator)) {
		return fmt.Errorf("unsafe path in pi archive: %q", header.Name)
	}
	switch header.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(path, modeOrDefault(header.FileInfo().Mode(), 0o755))
	case tar.TypeReg, tar.TypeRegA:
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("create archive dir: %w", err)
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, modeOrDefault(header.FileInfo().Mode(), 0o755))
		if err != nil {
			return fmt.Errorf("create archive file: %w", err)
		}
		_, copyErr := io.Copy(f, r)
		closeErr := f.Close()
		if copyErr != nil {
			return fmt.Errorf("extract archive file: %w", copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close archive file: %w", closeErr)
		}
		return nil
	default:
		return nil
	}
}

func modeOrDefault(mode, fallback os.FileMode) os.FileMode {
	if mode == 0 {
		return fallback
	}
	return mode
}

func verifyPiVersion(ctx context.Context, binPath, want string) error {
	cmd := exec.CommandContext(ctx, binPath, "--version")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("verify pi version: %w", err)
	}
	got := strings.TrimSpace(string(out))
	if got != want {
		return fmt.Errorf("verify pi version: got %q, want %q", got, want)
	}
	return nil
}

func replaceDir(targetDir, newDir string) error {
	backupDir := targetDir + ".old"
	if err := os.RemoveAll(backupDir); err != nil {
		return fmt.Errorf("remove old backup: %w", err)
	}
	targetExists := true
	if err := os.Rename(targetDir, backupDir); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("move existing pi install aside: %w", err)
		}
		targetExists = false
	}
	if err := os.Rename(newDir, targetDir); err != nil {
		if targetExists {
			_ = os.Rename(backupDir, targetDir)
		}
		return fmt.Errorf("install pi: %w", err)
	}
	if targetExists {
		if err := os.RemoveAll(backupDir); err != nil {
			return fmt.Errorf("remove replaced pi install: %w", err)
		}
	}
	return nil
}

func ensureUserBinLink(binDir, linkPath, binPath string) error {
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("create bin dir: %w", err)
	}
	if info, err := os.Lstat(linkPath); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("%s exists and is not a symlink", linkPath)
		}
		if err := os.Remove(linkPath); err != nil {
			return fmt.Errorf("replace pi symlink: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect pi symlink: %w", err)
	}
	if err := os.Symlink(binPath, linkPath); err != nil {
		return fmt.Errorf("create pi symlink: %w", err)
	}
	return nil
}
