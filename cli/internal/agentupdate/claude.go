package agentupdate

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
)

const (
	DefaultClaudeInstallPath = "/usr/local/bin/claude"
	defaultClaudeReleaseBase = "https://downloads.claude.ai/claude-code-releases"
)

func updateClaude(ctx context.Context, opts Options) (Result, error) {
	u := newUpdater(opts.HTTPClient, opts.Stdout)
	releaseBase := opts.ReleaseBase
	if releaseBase == "" {
		releaseBase = defaultClaudeReleaseBase
	}
	installPath := opts.InstallPath
	if installPath == "" {
		installPath = DefaultClaudeInstallPath
	}

	version, err := resolveClaudeVersion(ctx, u, releaseBase, opts.Version)
	if err != nil {
		return Result{}, err
	}
	platform, err := claudePlatform()
	if err != nil {
		return Result{}, err
	}
	wantChecksum, err := claudeChecksum(ctx, u, releaseBase, version, platform)
	if err != nil {
		return Result{}, err
	}
	body, err := u.get(ctx, joinURL(releaseBase, version, platform, "claude"), "download claude")
	if err != nil {
		return Result{}, err
	}
	defer body.Close()

	exe, err := writeTempExecutable(installPath, ".claude-", body)
	if err != nil {
		return Result{}, err
	}
	if subtle.ConstantTimeCompare(exe.sha256, wantChecksum) != 1 {
		_ = os.Remove(exe.path)
		return Result{}, fmt.Errorf("verify claude checksum: got %s, want %s", hex.EncodeToString(exe.sha256), hex.EncodeToString(wantChecksum))
	}
	if err := installExecutable(installPath, exe, string(AgentClaude)); err != nil {
		return Result{}, err
	}
	fmt.Fprintf(u.stdout, "claude: updated to %s\n", version)
	return Result{Agent: AgentClaude, Version: version, Path: installPath}, nil
}

func resolveClaudeVersion(ctx context.Context, u updater, releaseBase, version string) (string, error) {
	version = strings.TrimSpace(version)
	if version != "" {
		return version, nil
	}
	return u.readText(ctx, joinURL(releaseBase, "latest"), "fetch claude latest version")
}

func claudeChecksum(ctx context.Context, u updater, releaseBase, version, platform string) ([]byte, error) {
	body, err := u.get(ctx, joinURL(releaseBase, version, "manifest.json"), "fetch claude manifest")
	if err != nil {
		return nil, err
	}
	defer body.Close()

	var manifest struct {
		Platforms map[string]struct {
			Checksum string `json:"checksum"`
		} `json:"platforms"`
	}
	if err := json.NewDecoder(body).Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode claude manifest: %w", err)
	}
	entry, ok := manifest.Platforms[platform]
	if !ok {
		return nil, fmt.Errorf("claude manifest missing platform %s", platform)
	}
	checksum, err := parseSHA256(entry.Checksum)
	if err != nil {
		return nil, fmt.Errorf("claude manifest checksum: %w", err)
	}
	return checksum, nil
}

func claudePlatform() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "linux-x64", nil
	case "arm64":
		return "linux-arm64", nil
	default:
		return "", fmt.Errorf("unsupported architecture: %s", runtime.GOARCH)
	}
}
