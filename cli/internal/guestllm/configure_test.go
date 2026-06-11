package guestllm

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
)

func TestConfigureClientWritesCodexConfigFromShelleyDiscovery(t *testing.T) {
	home := t.TempDir()
	fixture := &discoveryFixture{
		integrations: []reflectionIntegration{
			{Name: "agentllm", Type: "llm"},
			{Name: "notllm", Type: "git"},
		},
		catalogs: map[string]llmModelCatalog{
			"agentllm.int.exe.xyz": catalog(
				llmCatalogModel{ID: "openai/gpt-5.5", Provider: "openai", NativeID: "gpt-5.5", APIs: []string{"openai_responses"}},
				llmCatalogModel{ID: "anthropic/claude-opus-4-7", Provider: "anthropic", NativeID: "claude-opus-4-7", APIs: []string{"anthropic_messages"}},
			),
		},
	}

	result, err := ConfigureClient(context.Background(), ClientCodex, Options{
		HomeDir:    home,
		HTTPClient: fixture.client(t),
	})
	if err != nil {
		t.Fatalf("ConfigureClient: %v", err)
	}
	requireResult(t, result, ClientCodex, "configured")
	if result.Default != "agentllm" {
		t.Fatalf("default = %q, want agentllm", result.Default)
	}

	codexConfig := readFile(t, filepath.Join(home, ".codex", "config.toml"))
	for _, want := range []string{
		`model_provider = "exe-agentllm"`,
		`[model_providers."exe-agentllm"]`,
		`name = "exe-agentllm"`,
		`base_url = "https://agentllm.int.exe.xyz/v1"`,
		`requires_openai_auth = false`,
	} {
		if !strings.Contains(codexConfig, want) {
			t.Fatalf("codex config missing %q:\n%s", want, codexConfig)
		}
	}
	for _, notWant := range []string{
		`model =`,
		`model_reasoning_effort`,
		`codex_default`,
		`openai_base_url`,
	} {
		if strings.Contains(codexConfig, notWant) {
			t.Fatalf("codex config includes %q:\n%s", notWant, codexConfig)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("configure codex wrote claude settings: %v", err)
	}

	state := readFile(t, statePath(home))
	if !strings.Contains(state, codexConfigKey) {
		t.Fatalf("state missing codex managed file hash:\n%s", state)
	}
	if strings.Contains(state, claudeSettingsKey) {
		t.Fatalf("configure codex wrote claude state:\n%s", state)
	}
}

func TestReflectionIntegrationsURLUsesDocumentedDefaultHost(t *testing.T) {
	if DefaultReflectionURL != "https://reflection.int.exe.xyz" {
		t.Fatalf("DefaultReflectionURL = %q", DefaultReflectionURL)
	}
	got, err := reflectionIntegrationsURL(DefaultReflectionURL)
	if err != nil {
		t.Fatalf("reflectionIntegrationsURL: %v", err)
	}
	if got != "https://reflection.int.exe.xyz/integrations" {
		t.Fatalf("integrations URL = %q", got)
	}
}

func TestConfigureClientWritesClaudeConfigFromShelleyDiscovery(t *testing.T) {
	home := t.TempDir()
	fixture := &discoveryFixture{
		integrations: []reflectionIntegration{{Name: "agentllm", Type: "llm"}},
		catalogs: map[string]llmModelCatalog{
			"agentllm.int.exe.xyz": catalog(
				llmCatalogModel{ID: "anthropic/claude-opus-4-7", Provider: "anthropic", NativeID: "claude-opus-4-7", APIs: []string{"anthropic_messages"}},
			),
		},
	}

	result, err := ConfigureClient(context.Background(), ClientClaudeCode, Options{
		HomeDir:    home,
		HTTPClient: fixture.client(t),
	})
	if err != nil {
		t.Fatalf("ConfigureClient: %v", err)
	}
	requireResult(t, result, ClientClaudeCode, "configured")

	var claude map[string]any
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(home, ".claude", "settings.json"))), &claude); err != nil {
		t.Fatalf("parse claude settings: %v", err)
	}
	if got := claude["apiKeyHelper"]; got != "printf implicit" {
		t.Fatalf("apiKeyHelper = %v, want printf implicit", got)
	}
	env, _ := claude["env"].(map[string]any)
	if _, ok := env["ANTHROPIC_API_KEY"]; ok {
		t.Fatalf("claude env should not set ANTHROPIC_API_KEY: %#v", env)
	}
	if got := env["ANTHROPIC_BASE_URL"]; got != "https://agentllm.int.exe.xyz" {
		t.Fatalf("ANTHROPIC_BASE_URL = %v, want integration base URL", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("configure claude wrote codex config: %v", err)
	}
}

func TestConfigureClientIgnoresUnsupportedCatalogs(t *testing.T) {
	home := t.TempDir()
	fixture := &discoveryFixture{
		integrations: []reflectionIntegration{
			{Name: "aaa", Type: "llm"},
			{Name: "agentllm", Type: "llm"},
		},
		catalogs: map[string]llmModelCatalog{
			"aaa.int.exe.xyz": catalog(
				llmCatalogModel{ID: "openai/text-embedding-3-small", Provider: "openai", NativeID: "text-embedding-3-small", APIs: []string{"openai_embeddings"}},
			),
			"agentllm.int.exe.xyz": catalog(
				llmCatalogModel{ID: "openai/gpt-5.5", Provider: "openai", NativeID: "gpt-5.5", APIs: []string{"openai_chat"}},
			),
		},
	}

	result, err := ConfigureClient(context.Background(), ClientCodex, Options{
		HomeDir:    home,
		HTTPClient: fixture.client(t),
	})
	if err != nil {
		t.Fatalf("ConfigureClient: %v", err)
	}
	requireResult(t, result, ClientCodex, "configured")
	if result.Default != "agentllm" {
		t.Fatalf("default = %q, want supported integration", result.Default)
	}
}

func TestConfigureClientErrorsWhenMultipleUsableIntegrationsExist(t *testing.T) {
	home := t.TempDir()
	fixture := &discoveryFixture{
		integrations: []reflectionIntegration{
			{Name: "agentllm", Type: "llm"},
			{Name: "otherllm", Type: "llm"},
		},
		catalogs: map[string]llmModelCatalog{
			"agentllm.int.exe.xyz": catalog(
				llmCatalogModel{ID: "openai/gpt-5.5", Provider: "openai", NativeID: "gpt-5.5", APIs: []string{"openai_responses"}},
			),
			"otherllm.int.exe.xyz": catalog(
				llmCatalogModel{ID: "openai/gpt-5.5", Provider: "openai", NativeID: "gpt-5.5", APIs: []string{"openai_responses"}},
			),
		},
	}

	_, err := ConfigureClient(context.Background(), ClientCodex, Options{
		HomeDir:    home,
		HTTPClient: fixture.client(t),
	})
	if err == nil {
		t.Fatal("ConfigureClient succeeded, want multiple-integration error")
	}
	for _, want := range []string{"multiple usable llm integrations for codex", "agentllm, otherllm", "--integration <name>"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err.Error(), want)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("codex config was written despite ambiguous integration: %v", err)
	}
}

func TestConfigureClientUsesRequestedIntegration(t *testing.T) {
	home := t.TempDir()
	fixture := &discoveryFixture{
		integrations: []reflectionIntegration{
			{Name: "agentllm", Type: "llm"},
			{Name: "otherllm", Type: "llm"},
		},
		catalogs: map[string]llmModelCatalog{
			"agentllm.int.exe.xyz": catalog(
				llmCatalogModel{ID: "openai/gpt-5.5", Provider: "openai", NativeID: "gpt-5.5", APIs: []string{"openai_responses"}},
			),
			"otherllm.int.exe.xyz": catalog(
				llmCatalogModel{ID: "openai/gpt-5.5", Provider: "openai", NativeID: "gpt-5.5", APIs: []string{"openai_responses"}},
			),
		},
	}

	result, err := ConfigureClient(context.Background(), ClientCodex, Options{
		HomeDir:         home,
		HTTPClient:      fixture.client(t),
		IntegrationName: "otherllm",
	})
	if err != nil {
		t.Fatalf("ConfigureClient: %v", err)
	}
	requireResult(t, result, ClientCodex, "configured")
	if result.Default != "otherllm" {
		t.Fatalf("default = %q, want requested integration", result.Default)
	}
	codexConfig := readFile(t, filepath.Join(home, ".codex", "config.toml"))
	if !strings.Contains(codexConfig, `model_provider = "exe-otherllm"`) ||
		!strings.Contains(codexConfig, `base_url = "https://otherllm.int.exe.xyz/v1"`) {
		t.Fatalf("codex config did not use requested integration:\n%s", codexConfig)
	}
}

func TestConfigureClientUsesTeamIntegrationHost(t *testing.T) {
	home := t.TempDir()
	fixture := &discoveryFixture{
		integrations: []reflectionIntegration{{Name: "teamllm", Type: "llm", Team: true}},
		catalogs: map[string]llmModelCatalog{
			"teamllm.team.int.exe.xyz": catalog(
				llmCatalogModel{ID: "openai/gpt-5.5", Provider: "openai", NativeID: "gpt-5.5", APIs: []string{"openai_responses"}},
			),
		},
	}

	result, err := ConfigureClient(context.Background(), ClientCodex, Options{
		HomeDir:    home,
		HTTPClient: fixture.client(t),
	})
	if err != nil {
		t.Fatalf("ConfigureClient: %v", err)
	}
	requireResult(t, result, ClientCodex, "configured")
	codexConfig := readFile(t, filepath.Join(home, ".codex", "config.toml"))
	if !strings.Contains(codexConfig, `base_url = "https://teamllm.team.int.exe.xyz/v1"`) {
		t.Fatalf("codex config does not use team integration host:\n%s", codexConfig)
	}
}

func TestConfigureClientSkipsWhenNoUsableIntegrationExists(t *testing.T) {
	fixture := &discoveryFixture{
		integrations: []reflectionIntegration{{Name: "agentllm", Type: "llm"}},
		catalogs: map[string]llmModelCatalog{
			"agentllm.int.exe.xyz": catalog(
				llmCatalogModel{ID: "openai/text-embedding-3-small", Provider: "openai", NativeID: "text-embedding-3-small", APIs: []string{"openai_embeddings"}},
			),
		},
	}

	result, err := ConfigureClient(context.Background(), ClientCodex, Options{
		HomeDir:    t.TempDir(),
		HTTPClient: fixture.client(t),
	})
	if err != nil {
		t.Fatalf("ConfigureClient: %v", err)
	}
	requireResult(t, result, ClientCodex, "skipped")
	if !strings.Contains(result.Detail, "no usable attached llm integration") {
		t.Fatalf("detail = %q, want no usable integration", result.Detail)
	}
}

func TestConfigureClientDoesNotOverwriteUserConfig(t *testing.T) {
	home := t.TempDir()
	userConfig := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(userConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userConfig, []byte("model_provider = \"mine\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := &discoveryFixture{
		integrations: []reflectionIntegration{{Name: "agentllm", Type: "llm"}},
		catalogs: map[string]llmModelCatalog{
			"agentllm.int.exe.xyz": catalog(
				llmCatalogModel{ID: "openai/gpt-5.5", Provider: "openai", NativeID: "gpt-5.5", APIs: []string{"openai_responses"}},
			),
		},
	}

	result, err := ConfigureClient(context.Background(), ClientCodex, Options{
		HomeDir:    home,
		HTTPClient: fixture.client(t),
	})
	if err != nil {
		t.Fatalf("ConfigureClient: %v", err)
	}
	requireResult(t, result, ClientCodex, "skipped")
	if got := readFile(t, userConfig); got != "model_provider = \"mine\"\n" {
		t.Fatalf("codex config overwritten:\n%s", got)
	}
}

func TestConfigureClientDoesNotClaimMatchingUserConfig(t *testing.T) {
	home := t.TempDir()
	userConfig := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(userConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	content, err := codexConfig("agentllm", "https://agentllm.int.exe.xyz")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userConfig, content, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := &discoveryFixture{
		integrations: []reflectionIntegration{{Name: "agentllm", Type: "llm"}},
		catalogs: map[string]llmModelCatalog{
			"agentllm.int.exe.xyz": catalog(
				llmCatalogModel{ID: "openai/gpt-5.5", Provider: "openai", NativeID: "gpt-5.5", APIs: []string{"openai_responses"}},
			),
		},
	}

	result, err := ConfigureClient(context.Background(), ClientCodex, Options{
		HomeDir:    home,
		HTTPClient: fixture.client(t),
	})
	if err != nil {
		t.Fatalf("ConfigureClient: %v", err)
	}
	requireResult(t, result, ClientCodex, "unchanged")
	if state := readFile(t, statePath(home)); strings.Contains(state, codexConfigKey) {
		t.Fatalf("state claims ownership of pre-existing matching user file:\n%s", state)
	}
}

func TestConfigureClientForgetsManagedConfigAfterUserEdit(t *testing.T) {
	home := t.TempDir()
	userConfig := filepath.Join(home, ".codex", "config.toml")
	fixture := &discoveryFixture{
		integrations: []reflectionIntegration{{Name: "agentllm", Type: "llm"}},
		catalogs: map[string]llmModelCatalog{
			"agentllm.int.exe.xyz": catalog(
				llmCatalogModel{ID: "openai/gpt-5.5", Provider: "openai", NativeID: "gpt-5.5", APIs: []string{"openai_responses"}},
			),
		},
	}

	if _, err := ConfigureClient(context.Background(), ClientCodex, Options{
		HomeDir:    home,
		HTTPClient: fixture.client(t),
	}); err != nil {
		t.Fatalf("first ConfigureClient: %v", err)
	}
	if state := readFile(t, statePath(home)); !strings.Contains(state, codexConfigKey) {
		t.Fatalf("state missing codex managed file hash:\n%s", state)
	}

	if err := os.WriteFile(userConfig, []byte("model_provider = \"mine\"\n"), 0o600); err != nil {
		t.Fatalf("write user config: %v", err)
	}
	result, err := ConfigureClient(context.Background(), ClientCodex, Options{
		HomeDir:    home,
		HTTPClient: fixture.client(t),
	})
	if err != nil {
		t.Fatalf("second ConfigureClient: %v", err)
	}
	requireResult(t, result, ClientCodex, "skipped")
	if got := readFile(t, userConfig); got != "model_provider = \"mine\"\n" {
		t.Fatalf("codex config overwritten:\n%s", got)
	}
	if state := readFile(t, statePath(home)); strings.Contains(state, codexConfigKey) {
		t.Fatalf("state still claims ownership after user edit:\n%s", state)
	}
}

func TestConfigureClientUpdatesPreviouslyManagedConfig(t *testing.T) {
	home := t.TempDir()
	fixture := &discoveryFixture{
		integrations: []reflectionIntegration{{Name: "agentllm", Type: "llm"}},
		catalogs: map[string]llmModelCatalog{
			"agentllm.int.exe.xyz": catalog(
				llmCatalogModel{ID: "openai/gpt-5.5", Provider: "openai", NativeID: "gpt-5.5", APIs: []string{"openai_responses"}},
			),
		},
	}
	client := fixture.client(t)

	if _, err := ConfigureClient(context.Background(), ClientCodex, Options{HomeDir: home, HTTPClient: client}); err != nil {
		t.Fatalf("first ConfigureClient: %v", err)
	}

	fixture.integrations = []reflectionIntegration{{Name: "newllm", Type: "llm"}}
	fixture.catalogs = map[string]llmModelCatalog{
		"newllm.int.exe.xyz": catalog(
			llmCatalogModel{ID: "openai/gpt-5.5", Provider: "openai", NativeID: "gpt-5.5", APIs: []string{"openai_responses"}},
		),
	}
	result, err := ConfigureClient(context.Background(), ClientCodex, Options{HomeDir: home, HTTPClient: client})
	if err != nil {
		t.Fatalf("second ConfigureClient: %v", err)
	}
	requireResult(t, result, ClientCodex, "updated")

	codexConfig := readFile(t, filepath.Join(home, ".codex", "config.toml"))
	if !strings.Contains(codexConfig, `model_provider = "exe-newllm"`) ||
		!strings.Contains(codexConfig, `base_url = "https://newllm.int.exe.xyz/v1"`) {
		t.Fatalf("codex config not updated:\n%s", codexConfig)
	}
}

func TestConfigurePrintsEveryRequestedClientResult(t *testing.T) {
	home := t.TempDir()
	fixture := &discoveryFixture{
		integrations: []reflectionIntegration{{Name: "agentllm", Type: "llm"}},
		catalogs: map[string]llmModelCatalog{
			"agentllm.int.exe.xyz": catalog(
				llmCatalogModel{ID: "openai/gpt-5.5", Provider: "openai", NativeID: "gpt-5.5", APIs: []string{"openai_responses"}},
				llmCatalogModel{ID: "anthropic/claude-opus-4-7", Provider: "anthropic", NativeID: "claude-opus-4-7", APIs: []string{"anthropic_messages"}},
			),
		},
	}
	var stdout bytes.Buffer

	results, err := Configure(context.Background(), []string{ClientCodex, ClientClaudeCode}, Options{
		HomeDir:    home,
		HTTPClient: fixture.client(t),
		Stdout:     &stdout,
	})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %#v, want two", results)
	}
	for _, want := range []string{"codex: configured", "claude: configured"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout = %q, want %q", stdout.String(), want)
		}
	}
}

type discoveryFixture struct {
	integrations []reflectionIntegration
	catalogs     map[string]llmModelCatalog
}

func (f *discoveryFixture) client(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case req.URL.Host == "reflection.int.exe.xyz" && req.URL.Path == "/integrations":
			return jsonResponse(t, req, reflectionResponse{Integrations: f.integrations}), nil
		case req.URL.Path == "/models.json":
			catalog, ok := f.catalogs[req.URL.Host]
			if !ok {
				t.Fatalf("unexpected models.json host %q", req.URL.Host)
			}
			return jsonResponse(t, req, catalog), nil
		default:
			t.Fatalf("unexpected discovery request: %s", req.URL.String())
			return nil, fmt.Errorf("unexpected discovery request: %s", req.URL.String())
		}
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func jsonResponse(t *testing.T, req *http.Request, body any) *http.Response {
	t.Helper()
	var b bytes.Buffer
	if err := json.NewEncoder(&b).Encode(body); err != nil {
		t.Fatalf("encode response: %v", err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(&b),
		Request:    req,
	}
}

func catalog(models ...llmCatalogModel) llmModelCatalog {
	return llmModelCatalog{
		SchemaVersion: 1,
		Models:        models,
	}
}

func requireResult(t *testing.T, result Result, client, status string) {
	t.Helper()
	if result.Client != client {
		t.Fatalf("client = %q, want %q (result: %#v)", result.Client, client, result)
	}
	if result.Status != status {
		t.Fatalf("%s status = %q, want %q (result: %#v)", client, result.Status, status, result)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
