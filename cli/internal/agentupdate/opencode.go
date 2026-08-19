package agentupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	DefaultOpenCodeInstallPath       = "/usr/local/bin/opencode"
	defaultOpenCodeLatestURL         = "https://api.github.com/repos/anomalyco/opencode/releases/latest"
	defaultOpenCodeReleaseBase       = "https://api.github.com/repos/anomalyco/opencode/releases/tags"
	maxOpenCodeArchiveSize     int64 = 256 << 20
	maxOpenCodeBinarySize      int64 = 512 << 20
)

type openCodeRelease struct {
	TagName string                 `json:"tag_name"`
	Assets  []openCodeReleaseAsset `json:"assets"`
}

type openCodeReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Digest             string `json:"digest"`
}

func updateOpenCode(ctx context.Context, opts Options) (Result, error) {
	u := newUpdater(opts.HTTPClient, opts.Stdout)
	latestURL := opts.LatestURL
	if latestURL == "" {
		latestURL = defaultOpenCodeLatestURL
	}
	releaseBase := opts.ReleaseBase
	if releaseBase == "" {
		releaseBase = defaultOpenCodeReleaseBase
	}
	installPath := opts.InstallPath
	if installPath == "" {
		installPath = DefaultOpenCodeInstallPath
	}

	releaseURL := latestURL
	requestedVersion := ""
	if strings.TrimSpace(opts.Version) != "" {
		version, err := normalizeOpenCodeVersion(opts.Version)
		if err != nil {
			return Result{}, err
		}
		requestedVersion = version
		releaseURL = joinURL(releaseBase, "v"+requestedVersion)
	}
	release, err := fetchOpenCodeRelease(ctx, u, releaseURL)
	if err != nil {
		return Result{}, err
	}
	version, err := normalizeOpenCodeVersion(release.TagName)
	if err != nil {
		return Result{}, fmt.Errorf("opencode release tag: %w", err)
	}
	if requestedVersion != "" && version != requestedVersion {
		return Result{}, fmt.Errorf("opencode release tag = %q, want v%s", release.TagName, requestedVersion)
	}

	assetName, err := openCodeAssetName(runtime.GOARCH)
	if err != nil {
		return Result{}, err
	}
	asset, err := findOpenCodeAsset(release, assetName)
	if err != nil {
		return Result{}, err
	}
	wantChecksum, err := parseSHA256(asset.Digest)
	if err != nil {
		if strings.TrimSpace(asset.Digest) == "" {
			return Result{}, fmt.Errorf("opencode release asset %s missing sha256 digest", assetName)
		}
		return Result{}, fmt.Errorf("opencode release asset %s digest: %w", assetName, err)
	}
	body, err := u.get(ctx, asset.BrowserDownloadURL, "download opencode")
	if err != nil {
		return Result{}, err
	}
	defer body.Close()

	if err := os.MkdirAll(filepath.Dir(installPath), 0o755); err != nil {
		return Result{}, fmt.Errorf("create opencode install dir: %w", err)
	}
	archive, err := writeBoundedOpenCodeArchive(filepath.Dir(installPath), body, maxOpenCodeArchiveSize)
	if err != nil {
		return Result{}, err
	}
	defer os.Remove(archive.path)
	if subtle.ConstantTimeCompare(archive.sha256, wantChecksum) != 1 {
		return Result{}, fmt.Errorf("verify opencode archive checksum: got %x, want %x", archive.sha256, wantChecksum)
	}

	exe, err := extractOpenCodeExecutable(installPath, archive.path)
	if err != nil {
		return Result{}, err
	}
	if err := installExecutable(installPath, exe, string(AgentOpenCode)); err != nil {
		return Result{}, err
	}
	fmt.Fprintf(u.stdout, "opencode: updated to %s\n", version)
	return Result{Agent: AgentOpenCode, Version: version, Path: installPath}, nil
}

func fetchOpenCodeRelease(ctx context.Context, u updater, releaseURL string) (openCodeRelease, error) {
	body, err := u.get(ctx, releaseURL, "fetch opencode release")
	if err != nil {
		return openCodeRelease{}, err
	}
	defer body.Close()
	var release openCodeRelease
	if err := json.NewDecoder(io.LimitReader(body, 4<<20)).Decode(&release); err != nil {
		return openCodeRelease{}, fmt.Errorf("decode opencode release: %w", err)
	}
	return release, nil
}

func findOpenCodeAsset(release openCodeRelease, name string) (openCodeReleaseAsset, error) {
	for _, asset := range release.Assets {
		if asset.Name != name {
			continue
		}
		if strings.TrimSpace(asset.BrowserDownloadURL) == "" {
			return asset, fmt.Errorf("opencode release asset %s missing download URL", name)
		}
		return asset, nil
	}
	return openCodeReleaseAsset{}, fmt.Errorf("opencode release missing asset %s", name)
}

func openCodeAssetName(goarch string) (string, error) {
	switch goarch {
	case "amd64":
		// The baseline build runs on older x86-64 CPUs without AVX2, so an
		// exeuntu image built on one host remains portable across the fleet.
		return "opencode-linux-x64-baseline.tar.gz", nil
	case "arm64":
		return "opencode-linux-arm64.tar.gz", nil
	default:
		return "", fmt.Errorf("unsupported architecture: %s", goarch)
	}
}

func normalizeOpenCodeVersion(value string) (string, error) {
	value = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "v"))
	if value == "" {
		return "", errors.New("opencode version is empty")
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '-' || r == '+' {
			continue
		}
		return "", fmt.Errorf("invalid opencode version %q", value)
	}
	return value, nil
}

func writeBoundedOpenCodeArchive(dir string, r io.Reader, maxBytes int64) (tempFile, error) {
	if maxBytes < 1 {
		return tempFile{}, errors.New("opencode archive size limit must be positive")
	}
	limited := &io.LimitedReader{R: r, N: maxBytes + 1}
	archive, err := writeTempFile(dir, ".opencode-archive-", limited)
	if err != nil {
		return tempFile{}, err
	}
	if limited.N == 0 {
		_ = os.Remove(archive.path)
		return tempFile{}, fmt.Errorf("download opencode: archive exceeds %d bytes", maxBytes)
	}
	return archive, nil
}

func extractOpenCodeExecutable(installPath, archivePath string) (tempExecutable, error) {
	archive, err := os.Open(archivePath)
	if err != nil {
		return tempExecutable{}, fmt.Errorf("open opencode archive: %w", err)
	}
	defer archive.Close()
	gzr, err := gzip.NewReader(archive)
	if err != nil {
		return tempExecutable{}, fmt.Errorf("read opencode archive: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	var executable tempExecutable
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			removeTempExecutable(executable)
			return tempExecutable{}, fmt.Errorf("read opencode archive: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(header.Name, "./"))
		if path.IsAbs(header.Name) || name == ".." || strings.HasPrefix(name, "../") {
			removeTempExecutable(executable)
			return tempExecutable{}, fmt.Errorf("extract opencode archive: invalid path %q", header.Name)
		}
		if name != "opencode" {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			removeTempExecutable(executable)
			return tempExecutable{}, errors.New("opencode archive entry is not a regular file")
		}
		if executable.path != "" {
			removeTempExecutable(executable)
			return tempExecutable{}, errors.New("opencode archive contains duplicate opencode entries")
		}
		if header.Size < 1 || header.Size > maxOpenCodeBinarySize {
			return tempExecutable{}, fmt.Errorf("opencode archive binary size %d is invalid", header.Size)
		}
		executable, err = writeTempExecutable(installPath, ".opencode-", tr)
		if err != nil {
			return tempExecutable{}, err
		}
	}
	if executable.path == "" {
		return tempExecutable{}, errors.New("opencode archive missing opencode executable")
	}
	return executable, nil
}

func removeTempExecutable(exe tempExecutable) {
	if exe.path != "" {
		_ = os.Remove(exe.path)
	}
}
