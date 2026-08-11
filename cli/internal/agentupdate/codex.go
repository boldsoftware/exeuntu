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
	codexCodeModeHostName   = "codex-code-mode-host"
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

	if err := os.MkdirAll(filepath.Dir(installPath), 0o755); err != nil {
		return Result{}, fmt.Errorf("create codex install dir: %w", err)
	}
	archive, err := writeTempFile(filepath.Dir(installPath), ".codex-archive-", body)
	if err != nil {
		return Result{}, err
	}
	defer os.Remove(archive.path)
	if subtle.ConstantTimeCompare(archive.sha256, checksum) != 1 {
		return Result{}, fmt.Errorf("verify codex archive checksum: got %x, want %x", archive.sha256, checksum)
	}
	packagePath, err := installCodexPackage(installPath, tag, assetArch, archive.sha256, archive.path)
	if err != nil {
		return Result{}, err
	}
	if err := activateCodexPackage(installPath, packagePath); err != nil {
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

func installCodexPackage(installPath, tag, assetArch string, checksum []byte, archivePath string) (string, error) {
	root := codexPackageRoot(installPath)
	releasesPath := filepath.Join(root, "releases")
	if err := os.MkdirAll(releasesPath, 0o755); err != nil {
		return "", fmt.Errorf("create codex releases dir: %w", err)
	}
	stagingPath, err := os.MkdirTemp(releasesPath, ".staging-")
	if err != nil {
		return "", fmt.Errorf("create codex package staging dir: %w", err)
	}
	defer os.RemoveAll(stagingPath)

	if err := extractCodexPackage(archivePath, stagingPath); err != nil {
		return "", err
	}
	if err := validateCodexPackage(stagingPath); err != nil {
		return "", err
	}

	releaseName := fmt.Sprintf("%s-%s-%x", sanitizeCodexReleaseName(tag), assetArch, checksum[:8])
	releasePath := filepath.Join(releasesPath, releaseName)
	if err := validateCodexPackage(releasePath); err != nil {
		if _, err := os.Stat(releasePath); err == nil {
			releasePath += "-repair-" + filepath.Base(stagingPath)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("stat existing codex package: %w", err)
		}
		if err := os.Rename(stagingPath, releasePath); err != nil {
			return "", fmt.Errorf("install codex package: %w", err)
		}
	}
	// The staging dir is created 0700 by MkdirTemp; the package root must be
	// world-traversable so non-root processes can run the activated binaries.
	if err := os.Chmod(releasePath, 0o755); err != nil {
		return "", fmt.Errorf("chmod codex package root: %w", err)
	}
	return releasePath, nil
}

func codexPackageRoot(installPath string) string {
	installDir := filepath.Dir(installPath)
	if filepath.Base(installDir) == "bin" {
		return filepath.Join(filepath.Dir(installDir), "lib", "codex")
	}
	return installPath + ".packages"
}

func sanitizeCodexReleaseName(tag string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '.', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, tag)
}

func extractCodexPackage(archivePath, packagePath string) error {
	archive, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open codex archive: %w", err)
	}
	defer archive.Close()
	gzr, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("read codex archive: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	var directories []struct {
		path string
		mode os.FileMode
	}
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read codex archive: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(header.Name, "./"))
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("extract codex archive: invalid path %q", header.Name)
		}
		destination := filepath.Join(packagePath, filepath.FromSlash(name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(destination, 0o755); err != nil {
				return fmt.Errorf("extract codex directory %s: %w", name, err)
			}
			directories = append(directories, struct {
				path string
				mode os.FileMode
			}{path: destination, mode: header.FileInfo().Mode().Perm()})
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
				return fmt.Errorf("create codex package directory for %s: %w", name, err)
			}
			file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, header.FileInfo().Mode().Perm())
			if err != nil {
				return fmt.Errorf("create codex package file %s: %w", name, err)
			}
			_, copyErr := io.Copy(file, tr)
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("extract codex package file %s: %w", name, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close codex package file %s: %w", name, closeErr)
			}
			if err := os.Chmod(destination, header.FileInfo().Mode().Perm()); err != nil {
				return fmt.Errorf("chmod codex package file %s: %w", name, err)
			}
		default:
			return fmt.Errorf("extract codex archive: unsupported entry %q", header.Name)
		}
	}
	for i := len(directories) - 1; i >= 0; i-- {
		if err := os.Chmod(directories[i].path, directories[i].mode); err != nil {
			return fmt.Errorf("chmod codex package directory: %w", err)
		}
	}
	return nil
}

func validateCodexPackage(packagePath string) error {
	binaryPath := filepath.Join(packagePath, "bin", "codex")
	info, err := os.Stat(binaryPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("codex archive missing bin/codex: %w", err)
		}
		return fmt.Errorf("stat codex package binary: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return errors.New("codex archive bin/codex is not executable")
	}
	return nil
}

func activateCodexPackage(installPath, packagePath string) error {
	if err := os.MkdirAll(filepath.Dir(installPath), 0o755); err != nil {
		return fmt.Errorf("create codex install dir: %w", err)
	}
	legacyPath := legacyLinkedUserBinary(installPath, string(AgentCodex))
	if err := atomicSymlink(filepath.Join(packagePath, "bin", "codex"), installPath); err != nil {
		return fmt.Errorf("activate codex package: %w", err)
	}
	// Flattened installs launched before this updater ran resolve the code
	// mode host by name next to the codex command; keep that path working
	// across upgrades by pointing it at the active package helper.
	helperTarget := filepath.Join(packagePath, "bin", codexCodeModeHostName)
	if _, err := os.Stat(helperTarget); err == nil {
		helperLink := filepath.Join(filepath.Dir(installPath), codexCodeModeHostName)
		if err := atomicSymlink(helperTarget, helperLink); err != nil {
			return fmt.Errorf("activate codex code mode host: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat codex code mode host: %w", err)
	}
	if legacyPath != "" {
		if err := os.Remove(legacyPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove legacy user binary: %w", err)
		}
	}
	return nil
}

// atomicSymlink points linkPath at target, replacing any existing file or
// symlink via rename so readers never observe a missing path.
func atomicSymlink(target, linkPath string) error {
	tmp, err := os.CreateTemp(filepath.Dir(linkPath), "."+filepath.Base(linkPath)+"-link-")
	if err != nil {
		return fmt.Errorf("create symlink path: %w", err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close symlink path: %w", err)
	}
	if err := os.Remove(tmpPath); err != nil {
		return fmt.Errorf("prepare symlink path: %w", err)
	}
	if err := os.Symlink(target, tmpPath); err != nil {
		return fmt.Errorf("create symlink: %w", err)
	}
	if err := os.Rename(tmpPath, linkPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace symlink: %w", err)
	}
	return nil
}
