package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestVersionHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"exeuntu", "version", "--help"}, &stdout, &stderr); err != nil {
		t.Fatalf("version --help: %v", err)
	}
	if !strings.Contains(stdout.String(), "usage: exeuntu version [options]") || !strings.Contains(stdout.String(), "--json") {
		t.Fatalf("stdout missing version usage:\n%s", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
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
				tc.command,
				"configure",
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
		"codex",
		"configure",
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

func TestUsageExposesConfigureShape(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"exeuntu", "help"}, &stdout, &stderr); err != nil {
		t.Fatalf("help: %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "codex") || !strings.Contains(got, "claude") {
		t.Fatalf("help missing configure commands:\n%s", got)
	}
	for _, want := range []string{
		"configure Codex to use the LLM integration",
		"configure Claude Code to use the LLM integration",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("help missing %q:\n%s", want, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "llm") || strings.Contains(stdout.String(), "configure codex") || strings.Contains(stdout.String(), "sync") || strings.Contains(stdout.String(), "llm-client") {
		t.Fatalf("help still exposes removed command:\n%s", stdout.String())
	}

	helpCases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "top long flag",
			args: []string{"exeuntu", "--help"},
			want: "usage: exeuntu <command>",
		},
		{
			name: "codex help command",
			args: []string{"exeuntu", "codex", "help"},
			want: "usage: exeuntu codex <command>",
		},
		{
			name: "codex help flag",
			args: []string{"exeuntu", "codex", "--help"},
			want: "usage: exeuntu codex <command>",
		},
		{
			name: "claude help command",
			args: []string{"exeuntu", "claude", "help"},
			want: "usage: exeuntu claude <command>",
		},
		{
			name: "claude help flag",
			args: []string{"exeuntu", "claude", "--help"},
			want: "usage: exeuntu claude <command>",
		},
		{
			name: "codex help flag",
			args: []string{"exeuntu", "codex", "configure", "--help"},
			want: "usage: exeuntu codex configure [options]",
		},
		{
			name: "claude help flag",
			args: []string{"exeuntu", "claude", "configure", "--help"},
			want: "usage: exeuntu claude configure [options]",
		},
	}
	for _, tc := range helpCases {
		t.Run(tc.name, func(t *testing.T) {
			stdout.Reset()
			stderr.Reset()
			if err := run(tc.args, &stdout, &stderr); err != nil {
				t.Fatalf("run %v: %v", tc.args, err)
			}
			if !strings.Contains(stdout.String(), tc.want) {
				t.Fatalf("stdout = %q, want %q", stdout.String(), tc.want)
			}
			if strings.Contains(tc.want, "[options]") && !strings.Contains(stdout.String(), "-integration string") {
				t.Fatalf("stdout missing integration option:\n%s", stdout.String())
			}
			if strings.Contains(tc.want, "<command>") && !strings.Contains(stdout.String(), "to use the LLM integration") {
				t.Fatalf("stdout missing LLM integration wording:\n%s", stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestUsageExposesPiUpdateShape(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"exeuntu", "help"}, &stdout, &stderr); err != nil {
		t.Fatalf("help: %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "pi") {
		t.Fatalf("help missing pi command:\n%s", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	helpCases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "pi help command",
			args: []string{"exeuntu", "pi", "help"},
			want: "usage: exeuntu pi <command>",
		},
		{
			name: "pi help flag",
			args: []string{"exeuntu", "pi", "--help"},
			want: "usage: exeuntu pi <command>",
		},
		{
			name: "pi update help flag",
			args: []string{"exeuntu", "pi", "update", "--help"},
			want: "usage: exeuntu pi update [options]",
		},
	}
	for _, tc := range helpCases {
		t.Run(tc.name, func(t *testing.T) {
			stdout.Reset()
			stderr.Reset()
			if err := run(tc.args, &stdout, &stderr); err != nil {
				t.Fatalf("run %v: %v", tc.args, err)
			}
			if !strings.Contains(stdout.String(), tc.want) {
				t.Fatalf("stdout = %q, want %q", stdout.String(), tc.want)
			}
			if tc.name == "pi update help flag" && !strings.Contains(stdout.String(), "-version string") {
				t.Fatalf("stdout missing version option:\n%s", stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestBareCommandGroupsPrintUsageWithoutDiagnostic(t *testing.T) {
	for _, args := range [][]string{
		{"exeuntu"},
		{"exeuntu", "codex"},
		{"exeuntu", "claude"},
		{"exeuntu", "pi"},
	} {
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(args, &stdout, &stderr)
			if !errors.Is(err, errUsage) {
				t.Fatalf("run err = %v, want errUsage", err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			got := stderr.String()
			if !strings.Contains(got, "usage:") {
				t.Fatalf("stderr missing usage:\n%s", got)
			}
			for _, unwanted := range []string{"missing command", "missing codex command", "missing claude command", "missing pi command"} {
				if strings.Contains(got, unwanted) {
					t.Fatalf("stderr contains diagnostic %q:\n%s", unwanted, got)
				}
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
		{"exeuntu", "configure", "codex"},
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
			if got := stderr.String(); !strings.Contains(got, "usage: exeuntu <command>") || strings.Contains(got, "llm") || strings.Contains(got, "configure codex") {
				t.Fatalf("stderr = %q, want top-level usage without removed commands", got)
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
