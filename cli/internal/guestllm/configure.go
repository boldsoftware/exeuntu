package guestllm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	DefaultReflectionURL = "https://reflection.int.exe.xyz"

	ClientCodex      = "codex"
	ClientClaudeCode = "claude"

	codexConfigKey    = "codex_config"
	claudeSettingsKey = "claude_settings"
	stateVersion      = 1
)

type Options struct {
	ReflectionURL      string
	HomeDir            string
	HTTPClient         *http.Client
	Stdout             io.Writer
	IntegrationName    string
	IntegrationBaseURL func(name string, team bool) string
}

type Result struct {
	Client  string
	Status  string
	Path    string
	Detail  string
	Default string
}

type reflectionResponse struct {
	Integrations []reflectionIntegration `json:"integrations"`
}

type reflectionIntegration struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Team bool   `json:"team"`
}

type llmModelCatalog struct {
	SchemaVersion int               `json:"schema_version"`
	Models        []llmCatalogModel `json:"models"`
}

type llmCatalogModel struct {
	ID       string   `json:"id"`
	Provider string   `json:"provider"`
	NativeID string   `json:"native_id"`
	APIs     []string `json:"apis"`
}

type discoveredIntegration struct {
	name           string
	baseURL        string
	supportsCodex  bool
	supportsClaude bool
}

type managedState struct {
	Version int               `json:"version"`
	Files   map[string]string `json:"files"`
}

func ConfigureClient(ctx context.Context, client string, opts Options) (Result, error) {
	results, err := Configure(ctx, []string{client}, opts)
	if len(results) > 0 {
		return results[0], err
	}
	return Result{}, err
}

func Configure(ctx context.Context, clients []string, opts Options) ([]Result, error) {
	if opts.ReflectionURL == "" {
		opts.ReflectionURL = DefaultReflectionURL
	}
	if opts.HomeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("home dir: %w", err)
		}
		opts.HomeDir = home
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	if opts.IntegrationBaseURL == nil {
		opts.IntegrationBaseURL = defaultIntegrationBaseURL
	}

	integrations, err := discoverIntegrations(ctx, opts)
	if err != nil {
		return nil, err
	}
	st, err := readState(opts.HomeDir)
	if err != nil {
		return nil, err
	}

	results := make([]Result, 0, len(clients))
	var errs []error
	for _, client := range clients {
		res, err := configureClient(opts.HomeDir, client, integrations, opts.IntegrationName, &st)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		results = append(results, res)
	}
	if err := writeState(opts.HomeDir, st); err != nil {
		errs = append(errs, err)
	}
	if opts.Stdout != nil {
		for _, res := range results {
			printResult(opts.Stdout, res)
		}
	}
	return results, errors.Join(errs...)
}

func configureClient(home, client string, integrations []discoveredIntegration, integrationName string, st *managedState) (Result, error) {
	if client != ClientCodex && client != ClientClaudeCode {
		return Result{}, fmt.Errorf("unsupported client %q", client)
	}
	integration, skip, err := selectClientIntegration(integrations, client, integrationName)
	if err != nil {
		return Result{}, err
	}
	if skip != "" {
		return Result{Client: client, Status: "skipped", Detail: skip}, nil
	}

	var content []byte
	var stateKey string
	var path string
	switch client {
	case ClientCodex:
		content, err = codexConfig(integration.name, integration.baseURL)
		stateKey = codexConfigKey
		path = filepath.Join(home, ".codex", "config.toml")
	case ClientClaudeCode:
		content, err = claudeSettings(integration.baseURL)
		stateKey = claudeSettingsKey
		path = filepath.Join(home, ".claude", "settings.json")
	default:
		return Result{}, fmt.Errorf("unsupported client %q", client)
	}
	if err != nil {
		return Result{}, err
	}
	res, err := applyManagedFile(path, stateKey, content, st)
	if err != nil {
		return Result{}, err
	}
	res.Client = client
	res.Default = integration.name
	return res, nil
}

func discoverIntegrations(ctx context.Context, opts Options) ([]discoveredIntegration, error) {
	reflection, err := fetchReflection(ctx, opts.HTTPClient, opts.ReflectionURL)
	if err != nil {
		return nil, err
	}

	candidates := make([]reflectionIntegration, 0, len(reflection.Integrations))
	for _, integration := range reflection.Integrations {
		if integration.Type == "llm" && strings.TrimSpace(integration.Name) != "" {
			integration.Name = strings.TrimSpace(integration.Name)
			candidates = append(candidates, integration)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Name != candidates[j].Name {
			return candidates[i].Name < candidates[j].Name
		}
		return !candidates[i].Team && candidates[j].Team
	})

	out := make([]discoveredIntegration, 0, len(candidates))
	for _, candidate := range candidates {
		baseURL := strings.TrimRight(opts.IntegrationBaseURL(candidate.Name, candidate.Team), "/")
		catalog, err := fetchModelCatalog(ctx, opts.HTTPClient, baseURL)
		if err != nil {
			continue
		}
		discovered := discoveredIntegration{name: candidate.Name, baseURL: baseURL}
		for _, model := range catalog.Models {
			if model.apiModelName() == "" {
				continue
			}
			switch model.Provider {
			case "anthropic":
				discovered.supportsClaude = discovered.supportsClaude || hasAPI(model.APIs, "anthropic_messages")
			case "openai":
				discovered.supportsCodex = discovered.supportsCodex || hasAnyAPI(model.APIs, "openai_responses", "openai_chat")
			case "fireworks":
				discovered.supportsCodex = discovered.supportsCodex || hasAPI(model.APIs, "openai_chat")
			}
		}
		if discovered.supportsCodex || discovered.supportsClaude {
			out = append(out, discovered)
		}
	}
	return out, nil
}

func (m llmCatalogModel) apiModelName() string {
	if m.NativeID != "" {
		return m.NativeID
	}
	return m.ID
}

func fetchReflection(ctx context.Context, client *http.Client, reflectionURL string) (reflectionResponse, error) {
	integrationsURL, err := reflectionIntegrationsURL(reflectionURL)
	if err != nil {
		return reflectionResponse{}, err
	}
	var out reflectionResponse
	if err := fetchJSON(ctx, client, integrationsURL, &out); err != nil {
		return reflectionResponse{}, fmt.Errorf("fetch reflection integrations: %w", err)
	}
	return out, nil
}

func fetchModelCatalog(ctx context.Context, client *http.Client, baseURL string) (llmModelCatalog, error) {
	if err := validateConfigURL(baseURL); err != nil {
		return llmModelCatalog{}, err
	}
	var catalog llmModelCatalog
	if err := fetchJSON(ctx, client, baseURL+"/models.json", &catalog); err != nil {
		return llmModelCatalog{}, err
	}
	if catalog.SchemaVersion != 1 {
		return llmModelCatalog{}, fmt.Errorf("unsupported schema version %d", catalog.SchemaVersion)
	}
	return catalog, nil
}

func fetchJSON(ctx context.Context, client *http.Client, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1024*1024))
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

func reflectionIntegrationsURL(reflectionURL string) (string, error) {
	u, err := url.Parse(reflectionURL)
	if err != nil {
		return "", fmt.Errorf("reflection URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("reflection URL must be absolute: %q", reflectionURL)
	}
	if strings.TrimRight(u.Path, "/") != "/integrations" {
		u.Path = strings.TrimRight(u.Path, "/") + "/integrations"
	}
	return u.String(), nil
}

func defaultIntegrationBaseURL(name string, team bool) string {
	host := name + ".int.exe.xyz"
	if team {
		host = name + ".team.int.exe.xyz"
	}
	return "https://" + host
}

func hasAPI(apis []string, want string) bool {
	for _, api := range apis {
		if api == want {
			return true
		}
	}
	return false
}

func hasAnyAPI(apis []string, wants ...string) bool {
	for _, want := range wants {
		if hasAPI(apis, want) {
			return true
		}
	}
	return false
}

func selectClientIntegration(integrations []discoveredIntegration, client, integrationName string) (discoveredIntegration, string, error) {
	integrationName = strings.TrimSpace(integrationName)
	var available []discoveredIntegration
	for _, integration := range integrations {
		if integrationName != "" && integration.name != integrationName {
			continue
		}
		switch client {
		case ClientCodex:
			if integration.supportsCodex {
				available = append(available, integration)
			}
		case ClientClaudeCode:
			if integration.supportsClaude {
				available = append(available, integration)
			}
		}
	}
	if len(available) == 0 {
		if integrationName != "" {
			return discoveredIntegration{}, fmt.Sprintf("selected llm integration %q is not usable for %s", integrationName, client), nil
		}
		return discoveredIntegration{}, "no usable attached llm integration", nil
	}
	if len(available) > 1 {
		return discoveredIntegration{}, "", fmt.Errorf(
			"multiple usable llm integrations for %s: %s; rerun with --integration <name>",
			client,
			discoveredIntegrationNames(available),
		)
	}
	return available[0], "", nil
}

func discoveredIntegrationNames(integrations []discoveredIntegration) string {
	names := make([]string, 0, len(integrations))
	for _, integration := range integrations {
		names = append(names, integration.name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func codexConfig(integrationName, baseURL string) ([]byte, error) {
	providerName := "exe-" + strings.TrimSpace(integrationName)
	if err := validateConfigText("codex provider name", providerName); err != nil {
		return nil, err
	}
	if err := validateConfigURL(baseURL); err != nil {
		return nil, fmt.Errorf("codex base url: %w", err)
	}
	var b bytes.Buffer
	b.WriteString("# Managed by exe.dev. Run `exeuntu configure codex` to refresh.\n")
	fmt.Fprintf(&b, "model_provider = %s\n", quoteTOMLString(providerName))
	b.WriteByte('\n')
	fmt.Fprintf(&b, "[model_providers.%s]\n", quoteTOMLString(providerName))
	fmt.Fprintf(&b, "name = %s\n", quoteTOMLString(providerName))
	fmt.Fprintf(&b, "base_url = %s\n", quoteTOMLString(strings.TrimRight(baseURL, "/")+"/v1"))
	b.WriteString("requires_openai_auth = false\n")
	return b.Bytes(), nil
}

func claudeSettings(baseURL string) ([]byte, error) {
	if err := validateConfigURL(baseURL); err != nil {
		return nil, fmt.Errorf("claude-code base url: %w", err)
	}
	body := map[string]any{
		"apiKeyHelper": "printf implicit",
		"env": map[string]string{
			"ANTHROPIC_BASE_URL": strings.TrimRight(baseURL, "/"),
		},
	}
	data, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func validateConfigURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("empty")
	}
	if err := validateConfigText("url", raw); err != nil {
		return err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("must start with http:// or https://")
	}
	if u.Host == "" {
		return errors.New("must include host")
	}
	return nil
}

func validateConfigText(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s: empty", name)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s: must not contain control characters", name)
		}
	}
	return nil
}

func quoteTOMLString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func applyManagedFile(path, stateKey string, content []byte, st *managedState) (Result, error) {
	if st.Files == nil {
		st.Files = map[string]string{}
	}
	desiredHash := sha256Hex(content)
	current, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := writeFile(path, content); err != nil {
			return Result{}, err
		}
		st.Files[stateKey] = desiredHash
		return Result{Status: "configured", Path: path}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("read %s: %w", path, err)
	}

	currentHash := sha256Hex(current)
	recordedHash := st.Files[stateKey]
	if currentHash == desiredHash {
		if recordedHash != "" {
			st.Files[stateKey] = desiredHash
		}
		return Result{Status: "unchanged", Path: path}, nil
	}

	// Ownership is content-addressed: exe.dev may update a file only when the
	// current bytes still match the last bytes exe.dev wrote. This avoids
	// marker comments becoming overwrite authority after a user takes over the
	// file. The tradeoff is conservative: user formatting or semantically
	// equivalent edits also stop automatic updates, which is the right failure
	// mode for developer-owned client config.
	if recordedHash != "" && recordedHash == currentHash {
		if err := writeFile(path, content); err != nil {
			return Result{}, err
		}
		st.Files[stateKey] = desiredHash
		return Result{Status: "updated", Path: path}, nil
	}
	delete(st.Files, stateKey)
	return Result{Status: "skipped", Path: path, Detail: "existing user config"}, nil
}

func readState(home string) (managedState, error) {
	st := managedState{
		Version: stateVersion,
		Files:   map[string]string{},
	}
	data, err := os.ReadFile(statePath(home))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return managedState{Version: stateVersion, Files: map[string]string{}}, nil
	}
	if st.Version != stateVersion {
		st.Version = stateVersion
	}
	if st.Files == nil {
		st.Files = map[string]string{}
	}
	return st, nil
}

func writeState(home string, st managedState) error {
	st.Version = stateVersion
	if st.Files == nil {
		st.Files = map[string]string{}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFile(statePath(home), data)
}

func statePath(home string) string {
	return filepath.Join(home, ".config", "exe", "guest-llm-config.json")
}

func writeFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp %s: %w", path, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp %s: %w", path, err)
	}
	return nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func printResult(w io.Writer, result Result) {
	switch {
	case result.Default != "" && result.Path != "":
		fmt.Fprintf(w, "%s: %s %s from %s", result.Client, result.Status, result.Path, result.Default)
		if result.Detail != "" {
			fmt.Fprintf(w, " (%s)", result.Detail)
		}
		fmt.Fprintln(w)
	case result.Path != "":
		fmt.Fprintf(w, "%s: %s %s", result.Client, result.Status, result.Path)
		if result.Detail != "" {
			fmt.Fprintf(w, " (%s)", result.Detail)
		}
		fmt.Fprintln(w)
	default:
		fmt.Fprintf(w, "%s: %s", result.Client, result.Status)
		if result.Detail != "" {
			fmt.Fprintf(w, ": %s", result.Detail)
		}
		fmt.Fprintln(w)
	}
}
