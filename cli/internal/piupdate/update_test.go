package piupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateInstallsLatestPiFromNPMVersionAndGitHubAssetShape(t *testing.T) {
	assetArch, err := piAssetArch()
	if err != nil {
		t.Fatal(err)
	}
	archive := piArchive(t, "1.2.3")
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		switch r.URL.Path {
		case "/latest":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"version":"1.2.3"}`)
		case "/v1.2.3/pi-linux-" + assetArch + ".tar.gz":
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	home := t.TempDir()
	var stdout bytes.Buffer
	result, err := Update(context.Background(), Options{
		HomeDir:     home,
		HTTPClient:  server.Client(),
		LatestURL:   server.URL + "/latest",
		ReleaseBase: server.URL,
		Stdout:      &stdout,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	if result.Version != "1.2.3" {
		t.Fatalf("version = %q, want 1.2.3", result.Version)
	}
	if got, want := stdout.String(), "pi: updated to 1.2.3\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	for _, want := range []string{"/latest", "/v1.2.3/pi-linux-" + assetArch + ".tar.gz"} {
		if !contains(requested, want) {
			t.Fatalf("requests = %v, want %s", requested, want)
		}
	}
	assertExecutableVersion(t, filepath.Join(home, ".local", "pi", "pi"), "1.2.3")
	link := filepath.Join(home, ".local", "bin", "pi")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("read pi symlink: %v", err)
	}
	if target != filepath.Join(home, ".local", "pi", "pi") {
		t.Fatalf("pi symlink target = %q", target)
	}
}

func TestUpdateVersionOverrideSkipsLatestLookup(t *testing.T) {
	assetArch, err := piAssetArch()
	if err != nil {
		t.Fatal(err)
	}
	archive := piArchive(t, "2.0.0")
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		if r.URL.Path != "/v2.0.0/pi-linux-"+assetArch+".tar.gz" {
			http.NotFound(w, r)
			return
		}
		w.Write(archive)
	}))
	defer server.Close()

	_, err = Update(context.Background(), Options{
		HomeDir:     t.TempDir(),
		Version:     "v2.0.0",
		HTTPClient:  server.Client(),
		LatestURL:   server.URL + "/latest",
		ReleaseBase: server.URL,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if contains(requested, "/latest") {
		t.Fatalf("requested latest URL despite version override: %v", requested)
	}
}

func TestUpdateRefusesToReplaceNonSymlinkUserBin(t *testing.T) {
	assetArch, err := piAssetArch()
	if err != nil {
		t.Fatal(err)
	}
	archive := piArchive(t, "1.2.3")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.2.3/pi-linux-"+assetArch+".tar.gz" {
			w.Write(archive)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "pi"), []byte("user file"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Update(context.Background(), Options{
		HomeDir:     home,
		Version:     "1.2.3",
		HTTPClient:  server.Client(),
		ReleaseBase: server.URL,
	})
	if err == nil || !strings.Contains(err.Error(), "exists and is not a symlink") {
		t.Fatalf("update err = %v, want non-symlink error", err)
	}
}

func piArchive(t *testing.T, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	writeTarFile(t, tw, "pi/pi", 0o755, "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo "+version+"; else echo pi; fi\n")
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func writeTarFile(t *testing.T, tw *tar.Writer, name string, mode int64, body string) {
	t.Helper()
	header := &tar.Header{
		Name: name,
		Mode: mode,
		Size: int64(len(body)),
	}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatalf("write body: %v", err)
	}
}

func assertExecutableVersion(t *testing.T, path, version string) {
	t.Helper()
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed pi: %v", err)
	}
	if !strings.Contains(string(out), "echo "+version) {
		t.Fatalf("installed pi script = %q, want version %s", string(out), version)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat installed pi: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("installed pi is not executable: %v", info.Mode())
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
