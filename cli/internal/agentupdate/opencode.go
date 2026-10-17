package agentupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
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

// OpenCode 2 ships each platform's native CLI as an npm package; the npm
// registry metadata carries the tarball URL and its sha512 integrity.
const (
	DefaultOpenCodeInstallPath       = "/usr/local/bin/opencode"
	defaultOpenCodeRegistry          = "https://registry.npmjs.org"
	openCodeArchiveBinary            = "package/bin/opencode"
	maxOpenCodeArchiveSize     int64 = 256 << 20
	maxOpenCodeBinarySize      int64 = 512 << 20
)

const (
	defaultOpenCodeConfigDir    = "/etc/opencode"
	defaultOpenCodeLegacyPlugin = "/home/exedev/.config/opencode/plugins/exe-dev.js"
	// openCodePluginMarker appears in every exeuntu-shipped version of the
	// plugin, so a home-directory copy carrying it is ours to remove.
	openCodePluginMarker = "[opencode-exe-dev]"
)

// The exe.dev plugin and config ship with exeuntu so that updating the
// OpenCode binary also updates the plugin it needs. They are installed into
// OpenCode's system config directory, which applies to every user.
var (
	//go:embed opencode-plugin/exe-dev.js
	openCodePlugin []byte
	//go:embed opencode-plugin/opencode.json
	openCodeConfig []byte
)

type openCodePackage struct {
	Version string `json:"version"`
	Dist    struct {
		Tarball   string `json:"tarball"`
		Integrity string `json:"integrity"`
	} `json:"dist"`
}

func updateOpenCode(ctx context.Context, opts Options) (Result, error) {
	u := newUpdater(opts.HTTPClient, opts.Stdout)
	registry := opts.ReleaseBase
	if registry == "" {
		registry = defaultOpenCodeRegistry
	}
	installPath := opts.InstallPath
	if installPath == "" {
		installPath = DefaultOpenCodeInstallPath
	}
	pkgName, err := openCodePackageName(runtime.GOARCH)
	if err != nil {
		return Result{}, err
	}

	version := "latest"
	if strings.TrimSpace(opts.Version) != "" {
		version, err = normalizeOpenCodeVersion(opts.Version)
		if err != nil {
			return Result{}, err
		}
	}
	pkg, err := fetchOpenCodePackage(ctx, u, joinURL(registry, pkgName, version))
	if err != nil {
		return Result{}, err
	}
	if version != "latest" && pkg.Version != version {
		return Result{}, fmt.Errorf("opencode package version = %q, want %s", pkg.Version, version)
	}
	version = pkg.Version
	wantSum, err := parseSHA512Integrity(pkg.Dist.Integrity)
	if err != nil {
		return Result{}, fmt.Errorf("opencode package %s@%s sha512 integrity: %w", pkgName, version, err)
	}
	body, err := u.get(ctx, pkg.Dist.Tarball, "download opencode")
	if err != nil {
		return Result{}, err
	}
	defer body.Close()

	if err := os.MkdirAll(filepath.Dir(installPath), 0o755); err != nil {
		return Result{}, fmt.Errorf("create opencode install dir: %w", err)
	}
	hash := sha512.New()
	archive, err := writeBoundedOpenCodeArchive(filepath.Dir(installPath), io.TeeReader(body, hash), maxOpenCodeArchiveSize)
	if err != nil {
		return Result{}, err
	}
	defer os.Remove(archive.path)
	if subtle.ConstantTimeCompare(hash.Sum(nil), wantSum) != 1 {
		return Result{}, errors.New("verify opencode archive integrity: sha512 mismatch")
	}

	exe, err := extractOpenCodeExecutable(installPath, archive.path)
	if err != nil {
		return Result{}, err
	}
	if err := installExecutable(installPath, exe, string(AgentOpenCode)); err != nil {
		return Result{}, err
	}
	if err := installOpenCodeConfig(opts); err != nil {
		return Result{}, err
	}
	fmt.Fprintf(u.stdout, "opencode: updated to %s\n", version)
	return Result{Agent: AgentOpenCode, Version: version, Path: installPath}, nil
}

func installOpenCodeConfig(opts Options) error {
	dir := opts.OpenCodeConfigDir
	if dir == "" {
		dir = defaultOpenCodeConfigDir
	}
	if err := writeFileAtomic(filepath.Join(dir, "plugins", "exe-dev.js"), openCodePlugin); err != nil {
		return fmt.Errorf("install opencode plugin: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, "opencode.json"), openCodeConfig); err != nil {
		return fmt.Errorf("install opencode config: %w", err)
	}

	// OpenCode 2 cannot load the V1 plugin older images put in the home
	// directory, and logs an error for it on every start.
	legacy := opts.OpenCodeLegacyPlugin
	if legacy == "" {
		legacy = defaultOpenCodeLegacyPlugin
	}
	old, err := os.ReadFile(legacy)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read legacy opencode plugin: %w", err)
	}
	if !bytes.Contains(old, []byte(openCodePluginMarker)) {
		return nil
	}
	if err := os.Remove(legacy); err != nil {
		return fmt.Errorf("remove legacy opencode plugin: %w", err)
	}
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := writeTempFile(dir, "."+filepath.Base(path)+"-", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer os.Remove(tmp.path)
	if err := os.Chmod(tmp.path, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.path, path)
}

func fetchOpenCodePackage(ctx context.Context, u updater, url string) (openCodePackage, error) {
	body, err := u.get(ctx, url, "fetch opencode package")
	if err != nil {
		return openCodePackage{}, err
	}
	defer body.Close()
	var pkg openCodePackage
	if err := json.NewDecoder(io.LimitReader(body, 4<<20)).Decode(&pkg); err != nil {
		return openCodePackage{}, fmt.Errorf("decode opencode package: %w", err)
	}
	return pkg, nil
}

// parseSHA512Integrity parses an npm Subresource Integrity value.
func parseSHA512Integrity(value string) ([]byte, error) {
	encoded, ok := strings.CutPrefix(strings.TrimSpace(value), "sha512-")
	if !ok {
		return nil, fmt.Errorf("unsupported value %q", value)
	}
	sum, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	if len(sum) != sha512.Size {
		return nil, fmt.Errorf("got %d bytes, want %d", len(sum), sha512.Size)
	}
	return sum, nil
}

func openCodePackageName(goarch string) (string, error) {
	switch goarch {
	case "amd64":
		// The baseline build runs on older x86-64 CPUs without AVX2, so an
		// exeuntu image built on one host remains portable across the fleet.
		return "@opencode/cli-linux-x64-baseline", nil
	case "arm64":
		return "@opencode/cli-linux-arm64", nil
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
		if name != openCodeArchiveBinary {
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
