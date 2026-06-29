package agentupdate

import (
	"archive/tar"
	"bufio"
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
	DefaultCodexInstallPath = "/usr/local/bin/codex"
	defaultCodexLatestURL   = "https://github.com/openai/codex/releases/latest"
	defaultCodexReleaseBase = "https://github.com/openai/codex/releases/download"
)

func updateCodex(ctx context.Context, opts Options) (Result, error) {
	u := newUpdater(opts.HTTPClient, opts.Stdout)
	latestURL := opts.LatestURL
	if latestURL == "" {
		latestURL = defaultCodexLatestURL
	}
	releaseBase := opts.ReleaseBase
	if releaseBase == "" {
		releaseBase = defaultCodexReleaseBase
	}
	installPath := opts.InstallPath
	if installPath == "" {
		installPath = DefaultCodexInstallPath
	}

	tag, err := resolveCodexTag(ctx, u, latestURL, opts.Version)
	if err != nil {
		return Result{}, err
	}
	assetArch, err := codexAssetArch()
	if err != nil {
		return Result{}, err
	}
	assetName := "codex-package-" + assetArch + ".tar.gz"
	checksum, err := codexPackageChecksum(ctx, u, releaseBase, tag, assetName)
	if err != nil {
		return Result{}, err
	}
	body, err := u.get(ctx, joinURL(releaseBase, tag, assetName), "download codex")
	if err != nil {
		return Result{}, err
	}
	defer body.Close()

	archive, err := writeTempFile(filepath.Dir(installPath), ".codex-archive-", body)
	if err != nil {
		return Result{}, err
	}
	defer os.Remove(archive.path)
	if subtle.ConstantTimeCompare(archive.sha256, checksum) != 1 {
		return Result{}, fmt.Errorf("verify codex archive checksum: got %x, want %x", archive.sha256, checksum)
	}
	exe, err := extractCodexExecutable(installPath, archive.path)
	if err != nil {
		return Result{}, err
	}
	if err := installExecutable(installPath, exe, string(AgentCodex)); err != nil {
		return Result{}, err
	}
	fmt.Fprintf(u.stdout, "codex: updated to %s\n", tag)
	return Result{Agent: AgentCodex, Version: tag, Path: installPath}, nil
}

func codexPackageChecksum(ctx context.Context, u updater, releaseBase, tag, assetName string) ([]byte, error) {
	body, err := u.get(ctx, joinURL(releaseBase, tag, "codex-package_SHA256SUMS"), "download codex checksums")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	sum, err := codexPackageChecksumFromManifest(body, assetName)
	if err != nil {
		return nil, err
	}
	return sum, nil
}

func codexPackageChecksumFromManifest(r io.Reader, assetName string) ([]byte, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || fields[1] != assetName {
			continue
		}
		sum, err := parseSHA256(fields[0])
		if err != nil {
			return nil, fmt.Errorf("codex checksum for %s: %w", assetName, err)
		}
		return sum, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read codex checksums: %w", err)
	}
	return nil, fmt.Errorf("codex checksums missing %s", assetName)
}

func resolveCodexTag(ctx context.Context, u updater, latestURL, version string) (string, error) {
	version = strings.TrimSpace(version)
	if version != "" {
		return normalizeCodexTag(version), nil
	}
	body, err := u.get(ctx, latestURL, "fetch codex latest release")
	if err != nil {
		return "", err
	}
	defer body.Close()
	if tag := codexTagFromLatestResponse(body); tag != "" {
		return tag, nil
	}
	var latest struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(body).Decode(&latest); err != nil {
		return "", fmt.Errorf("decode codex latest release: %w", err)
	}
	tag := strings.TrimSpace(latest.TagName)
	if tag == "" {
		return "", errors.New("codex latest release response missing tag_name")
	}
	return tag, nil
}

type responseURL interface {
	ResponseURL() string
}

func codexTagFromLatestResponse(r io.Reader) string {
	body, ok := r.(responseURL)
	if !ok {
		return ""
	}
	u := body.ResponseURL()
	if u == "" {
		return ""
	}
	const marker = "/releases/tag/"
	i := strings.LastIndex(u, marker)
	if i == -1 {
		return ""
	}
	tag := strings.Trim(strings.TrimSpace(u[i+len(marker):]), "/")
	if tag == "" || strings.Contains(tag, "/") {
		return ""
	}
	return tag
}

func normalizeCodexTag(version string) string {
	version = strings.TrimSpace(version)
	switch {
	case strings.HasPrefix(version, "rust-v"):
		return version
	case strings.HasPrefix(version, "rust-"):
		return "rust-v" + strings.TrimPrefix(version, "rust-")
	case strings.HasPrefix(version, "v"):
		return "rust-" + version
	default:
		return "rust-v" + version
	}
}

func codexAssetArch() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64-unknown-linux-musl", nil
	case "arm64":
		return "aarch64-unknown-linux-musl", nil
	default:
		return "", fmt.Errorf("unsupported architecture: %s", runtime.GOARCH)
	}
}

func extractCodexExecutable(installPath, archivePath string) (tempExecutable, error) {
	archive, err := os.Open(archivePath)
	if err != nil {
		return tempExecutable{}, fmt.Errorf("open codex archive: %w", err)
	}
	defer archive.Close()
	gzr, err := gzip.NewReader(archive)
	if err != nil {
		return tempExecutable{}, fmt.Errorf("read codex archive: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return tempExecutable{}, fmt.Errorf("read codex archive: %w", err)
		}
		if !isCodexBinaryEntry(header) {
			continue
		}
		exe, err := writeTempExecutable(installPath, ".codex-", tr)
		if err != nil {
			return tempExecutable{}, err
		}
		return exe, nil
	}
	return tempExecutable{}, errors.New("codex archive missing bin/codex")
}

func isCodexBinaryEntry(header *tar.Header) bool {
	if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
		return false
	}
	name := path.Clean(header.Name)
	return name == "bin/codex"
}
