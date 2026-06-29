package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/boldsoftware/exe.dev/exeuntu/internal/agentupdate"
	"github.com/boldsoftware/exe.dev/exeuntu/internal/piupdate"
)

func TestVersionPrintsStampedGitVersion(t *testing.T) {
	withGitVersion(t, "test-version")

	var stdout, stderr bytes.Buffer
	if err := run([]string{"exeuntu", "version"}, &stdout, &stderr); err != nil {
		t.Fatalf("version: %v", err)
	}
	if got, want := stdout.String(), "exeuntu test-version\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestVersionJSONMode(t *testing.T) {
	withGitVersion(t, "test-version")

	for _, args := range [][]string{
		{"exeuntu", "version", "--json"},
		{"exeuntu", "--version", "--json"},
	} {
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := run(args, &stdout, &stderr); err != nil {
				t.Fatalf("run %v: %v", args, err)
			}
			var got struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatalf("stdout is not json: %v\n%s", err, stdout.String())
			}
			if got.Name != "exeuntu" || got.Version != "test-version" {
				t.Fatalf("version json = %#v, want exeuntu/test-version", got)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestConfigureSubcommandsWriteSelectedClient(t *testing.T) {
	for _, tc := range []struct {
		command     string
		output      string
		writtenPath string
		absentPath  string
	}{
		{
			command:     "codex",
			output:      "codex: configured",
			writtenPath: filepath.Join(".codex", "config.toml"),
			absentPath:  filepath.Join(".claude", "settings.json"),
		},
		{
			command:     "claude",
			output:      "claude: configured",
			writtenPath: filepath.Join(".claude", "settings.json"),
			absentPath:  filepath.Join(".codex", "config.toml"),
		},
	} {
		t.Run(tc.command, func(t *testing.T) {
			withLLMDiscoveryTransport(t)
			home := t.TempDir()
			var stdout, stderr bytes.Buffer
			err := run([]string{
				"exeuntu",
				"configure",
				tc.command,
				"--home", home,
			}, &stdout, &stderr)
			if err != nil {
				t.Fatalf("run configure %s: %v\nstderr:\n%s", tc.command, err, stderr.String())
			}
			if !strings.Contains(stdout.String(), tc.output) {
				t.Fatalf("stdout = %q, want %q", stdout.String(), tc.output)
			}
			if _, err := os.Stat(filepath.Join(home, tc.writtenPath)); err != nil {
				t.Fatalf("expected %s to be written: %v", tc.writtenPath, err)
			}
			if _, err := os.Stat(filepath.Join(home, tc.absentPath)); !os.IsNotExist(err) {
				t.Fatalf("expected %s to be absent, got err %v", tc.absentPath, err)
			}
		})
	}
}

func TestConfigureSubcommandSelectsIntegration(t *testing.T) {
	withLLMDiscoveryTransportResponses(
		t,
		[]map[string]any{
			{"name": "agentllm", "type": "llm"},
			{"name": "otherllm", "type": "llm"},
		},
		map[string][]map[string]any{
			"agentllm.int.exe.xyz": {
				{"id": "openai/gpt-5.5", "provider": "openai", "native_id": "gpt-5.5", "apis": []string{"openai_responses"}},
			},
			"otherllm.int.exe.xyz": {
				{"id": "openai/gpt-5.5", "provider": "openai", "native_id": "gpt-5.5", "apis": []string{"openai_responses"}},
				{"id": "anthropic/claude-opus-4-7", "provider": "anthropic", "native_id": "claude-opus-4-7", "apis": []string{"anthropic_messages"}},
			},
		},
	)
	home := t.TempDir()
	var stdout, stderr bytes.Buffer

	err := run([]string{
		"exeuntu",
		"configure",
		"codex",
		"--home", home,
		"--integration", "otherllm",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run configure codex: %v\nstderr:\n%s", err, stderr.String())
	}
	codexConfig, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatalf("read codex config: %v", err)
	}
	if !strings.Contains(string(codexConfig), `model_provider = "exe-otherllm"`) {
		t.Fatalf("codex config did not use selected integration:\n%s", codexConfig)
	}
}

func TestUpdateCommandsAreSilentOnSuccess(t *testing.T) {
	withAgentUpdater(t, func(_ context.Context, opts agentupdate.Options) (agentupdate.Result, error) {
		if opts.Stdout != nil {
			fmt.Fprintln(opts.Stdout, "agent output")
		}
		return agentupdate.Result{Agent: opts.Agent, Version: "test-version", Path: "test-path"}, nil
	})
	withPiUpdater(t, func(_ context.Context, opts piupdate.Options) (piupdate.Result, error) {
		if opts.Stdout != nil {
			fmt.Fprintln(opts.Stdout, "pi output")
		}
		return piupdate.Result{Version: "test-version", Path: "test-path"}, nil
	})

	for _, args := range [][]string{
		{"exeuntu", "update", "claude"},
		{"exeuntu", "update", "codex"},
		{"exeuntu", "update", "pi"},
	} {
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := run(args, &stdout, &stderr); err != nil {
				t.Fatalf("run %v: %v", args, err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestInstallCommandsShowUpdaterOutput(t *testing.T) {
	withAgentUpdater(t, func(_ context.Context, opts agentupdate.Options) (agentupdate.Result, error) {
		fmt.Fprintln(opts.Stdout, "agent output")
		return agentupdate.Result{Agent: opts.Agent, Version: "test-version", Path: "test-path"}, nil
	})
	withPiUpdater(t, func(_ context.Context, opts piupdate.Options) (piupdate.Result, error) {
		fmt.Fprintln(opts.Stdout, "pi output")
		return piupdate.Result{Version: "test-version", Path: "test-path"}, nil
	})

	for _, tc := range []struct {
		args []string
		want string
	}{
		{args: []string{"exeuntu", "install", "claude"}, want: "agent output\n"},
		{args: []string{"exeuntu", "install", "codex"}, want: "agent output\n"},
		{args: []string{"exeuntu", "install", "pi"}, want: "pi output\n"},
	} {
		t.Run(strings.Join(tc.args[1:], " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := run(tc.args, &stdout, &stderr); err != nil {
				t.Fatalf("run %v: %v", tc.args, err)
			}
			if got := stdout.String(); got != tc.want {
				t.Fatalf("stdout = %q, want %q", got, tc.want)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestReplacedUpdateCommandsAreNotExposed(t *testing.T) {
	for _, args := range [][]string{
		{"exeuntu", "codex", "update", "--help"},
		{"exeuntu", "claude", "update", "--help"},
		{"exeuntu", "pi", "--help"},
		{"exeuntu", "pi", "update", "--help"},
	} {
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(args, &stdout, &stderr)
			if err == nil {
				t.Fatal("replaced update command succeeded, want error")
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			if got := stderr.String(); strings.Contains(got, "usage: exeuntu codex update") ||
				strings.Contains(got, "usage: exeuntu claude update") ||
				strings.Contains(got, "usage: exeuntu pi update") {
				t.Fatalf("stderr exposes replaced updater usage:\n%s", got)
			}
		})
	}
}

func TestRemovedConfigureCommandsAreNotExposed(t *testing.T) {
	for _, args := range [][]string{
		{"exeuntu", "llm"},
		{"exeuntu", "llm", "configure", "all"},
		{"exeuntu", "llm", "configure", "codex"},
		{"exeuntu", "llm", "configure", "claude"},
		{"exeuntu", "codex", "--help"},
		{"exeuntu", "codex", "configure", "--help"},
		{"exeuntu", "claude", "--help"},
		{"exeuntu", "claude", "configure", "--help"},
	} {
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(args, &stdout, &stderr)
			if err == nil {
				t.Fatal("removed configure command succeeded, want error")
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			if got := stderr.String(); strings.Contains(got, "usage: exeuntu codex configure") ||
				strings.Contains(got, "usage: exeuntu claude configure") ||
				strings.Contains(got, "usage: exeuntu codex <command>") ||
				strings.Contains(got, "usage: exeuntu claude <command>") {
				t.Fatalf("stderr exposes replaced configure usage:\n%s", got)
			}
		})
	}
}

func withGitVersion(t *testing.T, version string) {
	t.Helper()
	old := gitVersion
	gitVersion = version
	t.Cleanup(func() {
		gitVersion = old
	})
}

func withAgentUpdater(t *testing.T, fn func(context.Context, agentupdate.Options) (agentupdate.Result, error)) {
	t.Helper()
	old := updateAgent
	updateAgent = fn
	t.Cleanup(func() {
		updateAgent = old
	})
}

func withPiUpdater(t *testing.T, fn func(context.Context, piupdate.Options) (piupdate.Result, error)) {
	t.Helper()
	old := updatePi
	updatePi = fn
	t.Cleanup(func() {
		updatePi = old
	})
}

func withLLMDiscoveryTransport(t *testing.T) {
	withLLMDiscoveryTransportResponses(
		t,
		[]map[string]any{{
			"name": "agentllm",
			"type": "llm",
		}},
		map[string][]map[string]any{
			"agentllm.int.exe.xyz": {
				{"id": "openai/gpt-5.5", "provider": "openai", "native_id": "gpt-5.5", "apis": []string{"openai_responses"}},
				{"id": "anthropic/claude-opus-4-7", "provider": "anthropic", "native_id": "claude-opus-4-7", "apis": []string{"anthropic_messages"}},
			},
		},
	)
}

func withLLMDiscoveryTransportResponses(t *testing.T, integrations []map[string]any, catalogs map[string][]map[string]any) {
	t.Helper()
	oldTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body any
		switch req.URL.Host + req.URL.Path {
		case "reflection.int.exe.xyz/integrations":
			body = map[string]any{"integrations": integrations}
		default:
			if req.URL.Path != "/models.json" {
				t.Fatalf("unexpected discovery request: %s", req.URL.String())
				return nil, fmt.Errorf("unexpected discovery request: %s", req.URL.String())
			}
			models, ok := catalogs[req.URL.Host]
			if !ok {
				t.Fatalf("unexpected discovery request: %s", req.URL.String())
				return nil, fmt.Errorf("unexpected discovery request: %s", req.URL.String())
			}
			body = map[string]any{
				"schema_version": 1,
				"models":         models,
			}
		}
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode response: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(&buf),
			Request:    req,
		}, nil
	})
	t.Cleanup(func() {
		http.DefaultTransport = oldTransport
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
