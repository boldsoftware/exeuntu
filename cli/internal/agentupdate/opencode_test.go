package agentupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOpenCodePackageName(t *testing.T) {
	for _, tc := range []struct {
		goarch string
		want   string
	}{
		{goarch: "amd64", want: "@opencode/cli-linux-x64-baseline"},
		{goarch: "arm64", want: "@opencode/cli-linux-arm64"},
	} {
		t.Run(tc.goarch, func(t *testing.T) {
			got, err := openCodePackageName(tc.goarch)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("package = %q, want %q", got, tc.want)
			}
		})
	}
	if _, err := openCodePackageName("riscv64"); err == nil || !strings.Contains(err.Error(), "unsupported architecture") {
		t.Fatalf("unsupported arch error = %v", err)
	}
}

func TestUpdateOpenCodeInstallsLatestPackage(t *testing.T) {
	archive := openCodeArchive(t, "package/bin/opencode", tar.TypeReg, shellBinary(t, "2.0.32"))
	registry := newOpenCodeRegistry(t, "latest", "2.0.32", archive, integrity(archive))
	opts := openCodeOptions(t, registry)
	var stdout bytes.Buffer
	opts.Stdout = &stdout
	// A stale plugin from an earlier exeuntu version must be replaced.
	stale := filepath.Join(opts.OpenCodeConfigDir, "plugins", "exe-dev.js")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(opts.OpenCodeLegacyPlugin, []byte(`console.warn("[opencode-exe-dev] old");`), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := Update(context.Background(), opts)
	if err != nil {
		t.Fatalf("update opencode: %v", err)
	}
	if result.Agent != AgentOpenCode || result.Version != "2.0.32" || result.Path != opts.InstallPath {
		t.Fatalf("result = %#v", result)
	}
	if got, want := stdout.String(), "opencode: updated to 2.0.32\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertRequests(t, registry.requested, []string{"/" + registry.pkg + "/latest", "/cli.tgz"})
	assertExecutableOutput(t, opts.InstallPath, "2.0.32\n")
	assertFile(t, stale, openCodePlugin, 0o644)
	assertFile(t, filepath.Join(opts.OpenCodeConfigDir, "opencode.json"), openCodeConfig, 0o644)
	if _, err := os.Stat(opts.OpenCodeLegacyPlugin); !os.IsNotExist(err) {
		t.Fatalf("legacy exe.dev plugin still present: %v", err)
	}
}

func TestUpdateOpenCodePinnedVersion(t *testing.T) {
	archive := openCodeArchive(t, "./package/bin/opencode", tar.TypeReg, shellBinary(t, "2.0.16"))
	registry := newOpenCodeRegistry(t, "2.0.16", "2.0.16", archive, integrity(archive))
	opts := openCodeOptions(t, registry)
	opts.Version = "v2.0.16"
	result, err := Update(context.Background(), opts)
	if err != nil {
		t.Fatalf("update opencode: %v", err)
	}
	if result.Version != "2.0.16" {
		t.Fatalf("version = %q, want 2.0.16", result.Version)
	}
	assertRequests(t, registry.requested, []string{"/" + registry.pkg + "/2.0.16", "/cli.tgz"})
	assertExecutableOutput(t, opts.InstallPath, "2.0.16\n")
}

func TestUpdateOpenCodeRejectsVersionMismatch(t *testing.T) {
	registry := newOpenCodeRegistry(t, "2.0.16", "2.0.17", nil, integrity([]byte("x")))
	opts := openCodeOptions(t, registry)
	opts.Version = "2.0.16"
	_, err := Update(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "want 2.0.16") {
		t.Fatalf("update err = %v, want version mismatch", err)
	}
}

func TestUpdateOpenCodeRejectsIntegrityMismatchWithoutReplacingBinary(t *testing.T) {
	archive := openCodeArchive(t, "package/bin/opencode", tar.TypeReg, shellBinary(t, "new"))
	registry := newOpenCodeRegistry(t, "latest", "2.0.32", archive, integrity([]byte("other")))
	opts := openCodeOptions(t, registry)
	if err := os.WriteFile(opts.InstallPath, shellBinary(t, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Update(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "verify opencode archive integrity") {
		t.Fatalf("update err = %v, want integrity error", err)
	}
	assertExecutableOutput(t, opts.InstallPath, "old\n")
}

func TestUpdateOpenCodeRejectsUnsafeOrMissingBinaryWithoutReplacingBinary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		entryName string
		entryType byte
		wantErr   string
	}{
		{name: "traversal", entryName: "../package/bin/opencode", entryType: tar.TypeReg, wantErr: "invalid path"},
		{name: "symlink", entryName: "package/bin/opencode", entryType: tar.TypeSymlink, wantErr: "not a regular file"},
		{name: "missing", entryName: "package/README.md", entryType: tar.TypeReg, wantErr: "missing opencode"},
		{name: "v1 layout", entryName: "opencode", entryType: tar.TypeReg, wantErr: "missing opencode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := openCodeArchive(t, tc.entryName, tc.entryType, []byte("payload"))
			registry := newOpenCodeRegistry(t, "latest", "2.0.32", archive, integrity(archive))
			opts := openCodeOptions(t, registry)
			if err := os.WriteFile(opts.InstallPath, shellBinary(t, "old"), 0o755); err != nil {
				t.Fatal(err)
			}
			_, err := Update(context.Background(), opts)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("update err = %v, want %q", err, tc.wantErr)
			}
			assertExecutableOutput(t, opts.InstallPath, "old\n")
		})
	}
}

func TestUpdateOpenCodeRequiresSHA512Integrity(t *testing.T) {
	for _, value := range []string{"", "sha1-AAAA", "sha512-!!!"} {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			registry := newOpenCodeRegistry(t, "latest", "2.0.32", nil, value)
			_, err := Update(context.Background(), openCodeOptions(t, registry))
			if err == nil || !strings.Contains(err.Error(), "sha512 integrity") {
				t.Fatalf("update err = %v, want integrity error", err)
			}
		})
	}
}

func TestUpdateOpenCodeKeepsUserOwnedPluginWithSameName(t *testing.T) {
	archive := openCodeArchive(t, "package/bin/opencode", tar.TypeReg, shellBinary(t, "2.0.32"))
	registry := newOpenCodeRegistry(t, "latest", "2.0.32", archive, integrity(archive))
	opts := openCodeOptions(t, registry)
	userPlugin := []byte("export default { id: \"mine\" }")
	if err := os.WriteFile(opts.OpenCodeLegacyPlugin, userPlugin, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(context.Background(), opts); err != nil {
		t.Fatalf("update opencode: %v", err)
	}
	assertFile(t, opts.OpenCodeLegacyPlugin, userPlugin, 0o644)
}

func TestEmbeddedOpenCodePluginCarriesLegacyMarker(t *testing.T) {
	if !bytes.Contains(openCodePlugin, []byte(openCodePluginMarker)) {
		t.Fatalf("embedded plugin lacks legacy-cleanup marker %q", openCodePluginMarker)
	}
	var cfg map[string]any
	if err := json.Unmarshal(openCodeConfig, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["update"] != "disable" {
		t.Fatalf("opencode.json update = %v, want disable", cfg["update"])
	}
}

func assertFile(t *testing.T, path string, want []byte, mode os.FileMode) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != mode {
		t.Fatalf("%s mode = %v, want %v", path, info.Mode().Perm(), mode)
	}
}

type openCodeRegistry struct {
	*httptest.Server
	pkg       string
	requested []string
}

// newOpenCodeRegistry serves npm registry metadata for this platform's
// OpenCode package at /<pkg>/<selector> and the tarball at /cli.tgz.
func newOpenCodeRegistry(t *testing.T, selector, version string, archive []byte, integrity string) *openCodeRegistry {
	t.Helper()
	pkg, err := openCodePackageName(runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	registry := &openCodeRegistry{pkg: pkg}
	registry.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.requested = append(registry.requested, r.URL.Path)
		switch r.URL.Path {
		case "/" + pkg + "/" + selector:
			w.Header().Set("Content-Type", "application/json")
			err := json.NewEncoder(w).Encode(map[string]any{
				"version": version,
				"dist": map[string]string{
					"tarball":   "http://" + r.Host + "/cli.tgz",
					"integrity": integrity,
				},
			})
			if err != nil {
				t.Errorf("encode package: %v", err)
			}
		case "/cli.tgz":
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(registry.Close)
	return registry
}

func openCodeOptions(t *testing.T, registry *openCodeRegistry) Options {
	t.Helper()
	return Options{
		Agent:                AgentOpenCode,
		InstallPath:          filepath.Join(t.TempDir(), "opencode"),
		HTTPClient:           registry.Client(),
		ReleaseBase:          registry.URL,
		OpenCodeConfigDir:    filepath.Join(t.TempDir(), "etc", "opencode"),
		OpenCodeLegacyPlugin: filepath.Join(t.TempDir(), "exe-dev.js"),
	}
}

func integrity(b []byte) string {
	sum := sha512.Sum512(b)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func openCodeArchive(t *testing.T, name string, entryType byte, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	header := &tar.Header{
		Name:     name,
		Mode:     0o755,
		Size:     int64(len(body)),
		Typeflag: entryType,
	}
	if entryType == tar.TypeSymlink {
		header.Size = 0
		header.Linkname = "/tmp/other"
	}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if header.Size > 0 {
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestWriteBoundedOpenCodeArchiveRejectsOversizeAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	_, err := writeBoundedOpenCodeArchive(dir, strings.NewReader("12345"), 4)
	if err == nil || !strings.Contains(err.Error(), "archive exceeds 4 bytes") {
		t.Fatalf("write archive error = %v", err)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary archive was not cleaned up: %v", entries)
	}
}

func TestNormalizeOpenCodeVersion(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "2.0.32", want: "2.0.32"},
		{in: " v2.0.32-beta.1 ", want: "2.0.32-beta.1"},
		{in: "../latest", wantErr: true},
		{in: "", wantErr: true},
	} {
		t.Run(fmt.Sprintf("%q", tc.in), func(t *testing.T) {
			got, err := normalizeOpenCodeVersion(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normalize = %q, want error", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("normalize = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
