package agentupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func TestOpenCodeAssetName(t *testing.T) {
	for _, tc := range []struct {
		goarch string
		want   string
	}{
		{goarch: "amd64", want: "opencode-linux-x64-baseline.tar.gz"},
		{goarch: "arm64", want: "opencode-linux-arm64.tar.gz"},
	} {
		t.Run(tc.goarch, func(t *testing.T) {
			got, err := openCodeAssetName(tc.goarch)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("asset = %q, want %q", got, tc.want)
			}
		})
	}
	if _, err := openCodeAssetName("riscv64"); err == nil || !strings.Contains(err.Error(), "unsupported architecture") {
		t.Fatalf("unsupported arch error = %v", err)
	}
}

func TestUpdateOpenCodeInstallsLatestReleaseAsset(t *testing.T) {
	assetName, err := openCodeAssetName(runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	archive := openCodeArchive(t, "opencode", tar.TypeReg, shellBinary(t, "1.18.18"))
	sum := sha256.Sum256(archive)
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		switch r.URL.Path {
		case "/latest":
			writeOpenCodeRelease(t, w, "v1.18.18", assetName, serverURL(r)+"/asset", "sha256:"+hex.EncodeToString(sum[:]))
		case "/asset":
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	installPath := filepath.Join(t.TempDir(), "opencode")
	var stdout bytes.Buffer
	result, err := Update(context.Background(), Options{
		Agent:       AgentOpenCode,
		InstallPath: installPath,
		HTTPClient:  server.Client(),
		LatestURL:   server.URL + "/latest",
		ReleaseBase: server.URL + "/tags",
		Stdout:      &stdout,
	})
	if err != nil {
		t.Fatalf("update opencode: %v", err)
	}
	if result.Agent != AgentOpenCode || result.Version != "1.18.18" || result.Path != installPath {
		t.Fatalf("result = %#v", result)
	}
	if got, want := stdout.String(), "opencode: updated to 1.18.18\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	assertRequests(t, requested, []string{"/latest", "/asset"})
	assertExecutableOutput(t, installPath, "1.18.18\n")
}

func TestUpdateOpenCodePinnedVersionUsesTagRelease(t *testing.T) {
	assetName, err := openCodeAssetName(runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	archive := openCodeArchive(t, "./opencode", tar.TypeReg, shellBinary(t, "1.17.0"))
	sum := sha256.Sum256(archive)
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		switch r.URL.Path {
		case "/tags/v1.17.0":
			writeOpenCodeRelease(t, w, "v1.17.0", assetName, serverURL(r)+"/asset", hex.EncodeToString(sum[:]))
		case "/asset":
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	installPath := filepath.Join(t.TempDir(), "opencode")
	result, err := Update(context.Background(), Options{
		Agent:       AgentOpenCode,
		Version:     "v1.17.0",
		InstallPath: installPath,
		HTTPClient:  server.Client(),
		LatestURL:   server.URL + "/latest",
		ReleaseBase: server.URL + "/tags",
	})
	if err != nil {
		t.Fatalf("update opencode: %v", err)
	}
	if result.Version != "1.17.0" {
		t.Fatalf("version = %q, want 1.17.0", result.Version)
	}
	assertRequests(t, requested, []string{"/tags/v1.17.0", "/asset"})
	assertExecutableOutput(t, installPath, "1.17.0\n")
}

func TestUpdateOpenCodeRejectsChecksumMismatchWithoutReplacingBinary(t *testing.T) {
	assetName, err := openCodeAssetName(runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	archive := openCodeArchive(t, "opencode", tar.TypeReg, shellBinary(t, "new"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tags/v1.18.18":
			writeOpenCodeRelease(t, w, "v1.18.18", assetName, serverURL(r)+"/asset", strings.Repeat("0", sha256.Size*2))
		case "/asset":
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	installPath := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(installPath, shellBinary(t, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = Update(context.Background(), Options{
		Agent:       AgentOpenCode,
		Version:     "1.18.18",
		InstallPath: installPath,
		HTTPClient:  server.Client(),
		ReleaseBase: server.URL + "/tags",
	})
	if err == nil || !strings.Contains(err.Error(), "verify opencode archive checksum") {
		t.Fatalf("update err = %v, want checksum error", err)
	}
	assertExecutableOutput(t, installPath, "old\n")
}

func TestUpdateOpenCodeRejectsUnsafeOrMissingBinaryWithoutReplacingBinary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		entryName string
		entryType byte
		wantErr   string
	}{
		{name: "traversal", entryName: "../opencode", entryType: tar.TypeReg, wantErr: "invalid path"},
		{name: "symlink", entryName: "opencode", entryType: tar.TypeSymlink, wantErr: "not a regular file"},
		{name: "missing", entryName: "README.md", entryType: tar.TypeReg, wantErr: "missing opencode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assetName, err := openCodeAssetName(runtime.GOARCH)
			if err != nil {
				t.Skip(err)
			}
			archive := openCodeArchive(t, tc.entryName, tc.entryType, []byte("payload"))
			sum := sha256.Sum256(archive)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/latest":
					writeOpenCodeRelease(t, w, "v1.18.18", assetName, serverURL(r)+"/asset", hex.EncodeToString(sum[:]))
				case "/asset":
					_, _ = w.Write(archive)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			installPath := filepath.Join(t.TempDir(), "opencode")
			if err := os.WriteFile(installPath, shellBinary(t, "old"), 0o755); err != nil {
				t.Fatal(err)
			}
			_, err = Update(context.Background(), Options{
				Agent:       AgentOpenCode,
				InstallPath: installPath,
				HTTPClient:  server.Client(),
				LatestURL:   server.URL + "/latest",
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("update err = %v, want %q", err, tc.wantErr)
			}
			assertExecutableOutput(t, installPath, "old\n")
		})
	}
}

func TestUpdateOpenCodeRequiresReleaseDigest(t *testing.T) {
	assetName, err := openCodeAssetName(runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeOpenCodeRelease(t, w, "v1.18.18", assetName, serverURL(r)+"/asset", "")
	}))
	defer server.Close()

	_, err = Update(context.Background(), Options{
		Agent:       AgentOpenCode,
		InstallPath: filepath.Join(t.TempDir(), "opencode"),
		HTTPClient:  server.Client(),
		LatestURL:   server.URL,
	})
	if err == nil || !strings.Contains(err.Error(), "missing sha256 digest") {
		t.Fatalf("update err = %v, want missing digest error", err)
	}
}

func writeOpenCodeRelease(t *testing.T, w http.ResponseWriter, tag, assetName, downloadURL, digest string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"tag_name": tag,
		"assets": []map[string]string{{
			"name":                 assetName,
			"browser_download_url": downloadURL,
			"digest":               digest,
		}},
	}); err != nil {
		t.Fatalf("encode release: %v", err)
	}
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
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
		{in: "1.18.18", want: "1.18.18"},
		{in: " v1.18.18-beta.1 ", want: "1.18.18-beta.1"},
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
