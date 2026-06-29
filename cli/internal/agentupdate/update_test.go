package agentupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestUpdateClaudeInstallsLatestReleaseWithManifestChecksum(t *testing.T) {
	platform, err := claudePlatform()
	if err != nil {
		t.Fatal(err)
	}
	binary := shellBinary(t, "claude 2.1.185")
	sum := sha256.Sum256(binary)
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		switch r.URL.Path {
		case "/latest":
			fmt.Fprintln(w, "2.1.185")
		case "/2.1.185/manifest.json":
			fmt.Fprintf(w, `{"platforms":{%q:{"checksum":%q}}}`, platform, hex.EncodeToString(sum[:]))
		case "/2.1.185/" + platform + "/claude":
			w.Write(binary)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	legacyPath := filepath.Join(t.TempDir(), ".local", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte("legacy"), 0o755); err != nil {
		t.Fatal(err)
	}
	installPath := filepath.Join(t.TempDir(), "claude")
	if err := os.Symlink(legacyPath, installPath); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	result, err := Update(context.Background(), Options{
		Agent:       AgentClaude,
		InstallPath: installPath,
		HTTPClient:  server.Client(),
		ReleaseBase: server.URL,
		Stdout:      &stdout,
	})
	if err != nil {
		t.Fatalf("update claude: %v", err)
	}

	if result.Agent != AgentClaude || result.Version != "2.1.185" || result.Path != installPath {
		t.Fatalf("result = %#v", result)
	}
	if got, want := stdout.String(), "claude: updated to 2.1.185\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertRequests(t, requested, []string{"/latest", "/2.1.185/manifest.json", "/2.1.185/" + platform + "/claude"})
	assertExecutableOutput(t, installPath, "claude 2.1.185\n")
	if _, err := os.Lstat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy claude target still exists: %v", err)
	}
}

func TestUpdateClaudeRejectsChecksumMismatchWithoutInstalling(t *testing.T) {
	platform, err := claudePlatform()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/1.0.0/manifest.json":
			fmt.Fprintf(w, `{"platforms":{%q:{"checksum":%q}}}`, platform, strings.Repeat("0", sha256.Size*2))
		case "/1.0.0/" + platform + "/claude":
			w.Write(shellBinary(t, "unexpected claude"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	installPath := filepath.Join(t.TempDir(), "claude")
	_, err = Update(context.Background(), Options{
		Agent:       AgentClaude,
		Version:     "1.0.0",
		InstallPath: installPath,
		HTTPClient:  server.Client(),
		ReleaseBase: server.URL,
	})
	if err == nil || !strings.Contains(err.Error(), "verify claude checksum") {
		t.Fatalf("update err = %v, want checksum error", err)
	}
	if _, statErr := os.Stat(installPath); !os.IsNotExist(statErr) {
		t.Fatalf("install path exists after failed checksum: %v", statErr)
	}
}

func TestUpdateCodexInstallsLatestGitHubReleaseAsset(t *testing.T) {
	assetArch, err := codexAssetArch()
	if err != nil {
		t.Fatal(err)
	}
	binary := shellBinary(t, "codex rust-v0.140.0")
	assetName := "codex-package-" + assetArch + ".tar.gz"
	archive := codexArchive(t, "bin/codex", binary)
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		switch r.URL.Path {
		case "/latest":
			fmt.Fprint(w, `{"tag_name":"rust-v0.140.0"}`)
		case "/rust-v0.140.0/codex-package_SHA256SUMS":
			fmt.Fprint(w, codexSHA256Sums(assetName, archive))
		case "/rust-v0.140.0/" + assetName:
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	installPath := filepath.Join(t.TempDir(), "codex")
	var stdout bytes.Buffer
	result, err := Update(context.Background(), Options{
		Agent:       AgentCodex,
		InstallPath: installPath,
		HTTPClient:  server.Client(),
		LatestURL:   server.URL + "/latest",
		ReleaseBase: server.URL,
		Stdout:      &stdout,
	})
	if err != nil {
		t.Fatalf("update codex: %v", err)
	}

	if result.Agent != AgentCodex || result.Version != "rust-v0.140.0" || result.Path != installPath {
		t.Fatalf("result = %#v", result)
	}
	if got, want := stdout.String(), "codex: updated to rust-v0.140.0\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertRequests(t, requested, []string{"/latest", "/rust-v0.140.0/codex-package_SHA256SUMS", "/rust-v0.140.0/" + assetName})
	assertExecutableOutput(t, installPath, "codex rust-v0.140.0\n")
}

func TestResolveCodexTagUsesGitHubLatestRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			http.Redirect(w, r, "/openai/codex/releases/tag/rust-v0.141.0", http.StatusFound)
		case "/openai/codex/releases/tag/rust-v0.141.0":
			fmt.Fprint(w, "<html>release page</html>")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	tag, err := resolveCodexTag(context.Background(), newUpdater(server.Client(), nil), server.URL+"/latest", "")
	if err != nil {
		t.Fatalf("resolve codex tag: %v", err)
	}
	if tag != "rust-v0.141.0" {
		t.Fatalf("tag = %q, want rust-v0.141.0", tag)
	}
}

func TestUpdateCodexVersionOverrideSkipsLatestLookupAndNormalizesTag(t *testing.T) {
	assetArch, err := codexAssetArch()
	if err != nil {
		t.Fatal(err)
	}
	assetName := "codex-package-" + assetArch + ".tar.gz"
	archive := codexArchive(t, "bin/codex", shellBinary(t, "codex"))
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		switch r.URL.Path {
		case "/rust-v0.140.0/codex-package_SHA256SUMS":
			fmt.Fprint(w, codexSHA256Sums(assetName, archive))
		case "/rust-v0.140.0/" + assetName:
			w.Write(archive)
		default:
			http.NotFound(w, r)
			return
		}
	}))
	defer server.Close()

	result, err := Update(context.Background(), Options{
		Agent:       AgentCodex,
		Version:     "0.140.0",
		InstallPath: filepath.Join(t.TempDir(), "codex"),
		HTTPClient:  server.Client(),
		LatestURL:   server.URL + "/latest",
		ReleaseBase: server.URL,
	})
	if err != nil {
		t.Fatalf("update codex: %v", err)
	}
	if result.Version != "rust-v0.140.0" {
		t.Fatalf("version = %q, want rust-v0.140.0", result.Version)
	}
	assertRequests(t, requested, []string{"/rust-v0.140.0/codex-package_SHA256SUMS", "/rust-v0.140.0/" + assetName})
}

func TestUpdateCodexRejectsArchiveWithoutPlatformBinary(t *testing.T) {
	assetArch, err := codexAssetArch()
	if err != nil {
		t.Fatal(err)
	}
	assetName := "codex-package-" + assetArch + ".tar.gz"
	archive := codexArchive(t, "codex-other-platform", shellBinary(t, "codex"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rust-v0.140.0/codex-package_SHA256SUMS":
			fmt.Fprint(w, codexSHA256Sums(assetName, archive))
		case "/rust-v0.140.0/" + assetName:
			w.Write(archive)
		default:
			http.NotFound(w, r)
			return
		}
	}))
	defer server.Close()

	installPath := filepath.Join(t.TempDir(), "codex")
	_, err = Update(context.Background(), Options{
		Agent:       AgentCodex,
		Version:     "rust-v0.140.0",
		InstallPath: installPath,
		HTTPClient:  server.Client(),
		ReleaseBase: server.URL,
	})
	if err == nil || !strings.Contains(err.Error(), "codex archive missing") {
		t.Fatalf("update err = %v, want missing binary error", err)
	}
	if _, statErr := os.Stat(installPath); !os.IsNotExist(statErr) {
		t.Fatalf("install path exists after failed archive extraction: %v", statErr)
	}
}

func TestUpdateCodexRejectsArchiveChecksumMismatch(t *testing.T) {
	assetArch, err := codexAssetArch()
	if err != nil {
		t.Fatal(err)
	}
	assetName := "codex-package-" + assetArch + ".tar.gz"
	archive := codexArchive(t, "bin/codex", shellBinary(t, "codex"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rust-v0.140.0/codex-package_SHA256SUMS":
			fmt.Fprintf(w, "%s  %s\n", strings.Repeat("0", sha256.Size*2), assetName)
		case "/rust-v0.140.0/" + assetName:
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	installPath := filepath.Join(t.TempDir(), "codex")
	_, err = Update(context.Background(), Options{
		Agent:       AgentCodex,
		Version:     "rust-v0.140.0",
		InstallPath: installPath,
		HTTPClient:  server.Client(),
		ReleaseBase: server.URL,
	})
	if err == nil || !strings.Contains(err.Error(), "verify codex archive checksum") {
		t.Fatalf("update err = %v, want checksum error", err)
	}
	if _, statErr := os.Stat(installPath); !os.IsNotExist(statErr) {
		t.Fatalf("install path exists after failed archive checksum: %v", statErr)
	}
}

func TestUpdateRejectsUnsupportedAgent(t *testing.T) {
	_, err := Update(context.Background(), Options{Agent: "other"})
	if err == nil || !strings.Contains(err.Error(), "unsupported agent updater") {
		t.Fatalf("update err = %v, want unsupported agent error", err)
	}
}

func shellBinary(t *testing.T, output string) []byte {
	t.Helper()
	return []byte("#!/bin/sh\nprintf '%s\\n' " + shellQuote(output) + "\n")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func codexArchive(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	header := &tar.Header{
		Name: name,
		Mode: 0o755,
		Size: int64(len(body)),
	}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatalf("write codex tar header: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("write codex tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func codexSHA256Sums(assetName string, archive []byte) string {
	sum := sha256.Sum256(archive)
	return fmt.Sprintf("%x  %s\n", sum, assetName)
}

func assertRequests(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
}

func assertExecutableOutput(t *testing.T, path, want string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat installed executable: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("installed executable is still a symlink")
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("installed executable is not executable: %v", info.Mode())
	}
	out, err := exec.Command(path).Output()
	if err != nil {
		t.Fatalf("run installed executable: %v", err)
	}
	if string(out) != want {
		t.Fatalf("executable output = %q, want %q", string(out), want)
	}
}
