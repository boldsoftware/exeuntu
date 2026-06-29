package agentupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Agent string

const (
	AgentClaude Agent = "claude"
	AgentCodex  Agent = "codex"
)

type Options struct {
	Agent       Agent
	Version     string
	InstallPath string
	HTTPClient  *http.Client
	Stdout      io.Writer
	LatestURL   string
	ReleaseBase string
}

type Result struct {
	Agent   Agent
	Version string
	Path    string
}

func Update(ctx context.Context, opts Options) (Result, error) {
	switch opts.Agent {
	case AgentClaude:
		return updateClaude(ctx, opts)
	case AgentCodex:
		return updateCodex(ctx, opts)
	default:
		return Result{}, fmt.Errorf("unsupported agent updater: %q", opts.Agent)
	}
}

type updater struct {
	client *http.Client
	stdout io.Writer
}

func newUpdater(client *http.Client, stdout io.Writer) updater {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if stdout == nil {
		stdout = io.Discard
	}
	return updater{
		client: client,
		stdout: stdout,
	}
}

func (u updater) get(ctx context.Context, url, label string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%s request: %w", label, err)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: HTTP %d", label, resp.StatusCode)
	}
	return responseBody{
		ReadCloser: resp.Body,
		url:        resp.Request.URL.String(),
	}, nil
}

func (u updater) readText(ctx context.Context, url, label string) (string, error) {
	body, err := u.get(ctx, url, label)
	if err != nil {
		return "", err
	}
	defer body.Close()
	var buf strings.Builder
	if _, err := io.Copy(&buf, body); err != nil {
		return "", fmt.Errorf("%s response: %w", label, err)
	}
	text := strings.TrimSpace(buf.String())
	if text == "" {
		return "", fmt.Errorf("%s response is empty", label)
	}
	return text, nil
}

type tempExecutable struct {
	path   string
	sha256 []byte
}

type responseBody struct {
	io.ReadCloser
	url string
}

func (b responseBody) ResponseURL() string {
	return b.url
}

func writeTempExecutable(installPath, tmpPrefix string, r io.Reader) (tempExecutable, error) {
	if strings.TrimSpace(installPath) == "" {
		return tempExecutable{}, errors.New("install path is required")
	}
	dir := filepath.Dir(installPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return tempExecutable{}, fmt.Errorf("create install dir: %w", err)
	}
	tmp, err := writeTempFile(dir, tmpPrefix, r)
	if err != nil {
		return tempExecutable{}, err
	}
	if err := os.Chmod(tmp.path, 0o755); err != nil {
		_ = os.Remove(tmp.path)
		return tempExecutable{}, fmt.Errorf("chmod temp executable: %w", err)
	}
	return tempExecutable{path: tmp.path, sha256: tmp.sha256}, nil
}

type tempFile struct {
	path   string
	sha256 []byte
}

func writeTempFile(dir, tmpPrefix string, r io.Reader) (tempFile, error) {
	tmp, err := os.CreateTemp(dir, tmpPrefix)
	if err != nil {
		return tempFile{}, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(tmp, hash), r)
	closeErr := tmp.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return tempFile{}, fmt.Errorf("write temp file: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return tempFile{}, fmt.Errorf("close temp file: %w", closeErr)
	}
	return tempFile{path: tmpPath, sha256: hash.Sum(nil)}, nil
}

func installExecutable(installPath string, exe tempExecutable, legacyName string) error {
	legacyPath := legacyLinkedUserBinary(installPath, legacyName)
	if err := os.Rename(exe.path, installPath); err != nil {
		_ = os.Remove(exe.path)
		return fmt.Errorf("install executable: %w", err)
	}
	if legacyPath == "" {
		return nil
	}
	if err := os.Remove(legacyPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove legacy user binary: %w", err)
	}
	return nil
}

func legacyLinkedUserBinary(installPath, name string) string {
	target, err := os.Readlink(installPath)
	if err != nil {
		return ""
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(installPath), target)
	}
	target = filepath.Clean(target)
	if filepath.Base(target) != name {
		return ""
	}
	binDir := filepath.Dir(target)
	localDir := filepath.Dir(binDir)
	if filepath.Base(binDir) != "bin" || filepath.Base(localDir) != ".local" {
		return ""
	}
	return target
}

func parseSHA256(value string) ([]byte, error) {
	value = strings.TrimSpace(strings.TrimPrefix(value, "sha256:"))
	sum, err := hex.DecodeString(value)
	if err != nil {
		return nil, err
	}
	if len(sum) != sha256.Size {
		return nil, fmt.Errorf("got %d bytes, want %d", len(sum), sha256.Size)
	}
	return sum, nil
}

func joinURL(base string, elems ...string) string {
	out := strings.TrimRight(base, "/")
	for _, elem := range elems {
		out += "/" + strings.Trim(elem, "/")
	}
	return out
}
