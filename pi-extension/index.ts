import {
  getAgentDir,
  type ExtensionAPI,
  type ExtensionContext,
  type ProviderConfig,
  type ProviderModelConfig,
} from "@mariozechner/pi-coding-agent";
import { findEnvKeys } from "@mariozechner/pi-ai";
import { existsSync, mkdirSync, readFileSync, renameSync, unlinkSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

// LLM gateway base, reachable inside every exe.dev VM. The metadata server
// rewrites /gateway/llm/X to /_/gateway/X (exelet/metadata/metadata.go) and
// forwards to the gateway, which serves the catalog at /_/gateway/models.json
// before the credit check (llmgateway/gateway.go).
const GATEWAY = "http://169.254.169.254/gateway/llm";
const CATALOG_URL = `${GATEWAY}/models.json`;

// Catalog freshness strategy: the on-disk cache is the preferred source;
// the bundled catalog.json shipped with the extension image is the fallback;
// after registering providers we refresh the cache in the background so the
// next launch picks up new models, pricing, or compat hints.
const HERE = dirname(fileURLToPath(import.meta.url));
const BUNDLED_CATALOG = join(HERE, "catalog.json");
const CACHE_DIR = join(homedir(), ".cache", "pi-exe-dev");
const CACHE_FILE = join(CACHE_DIR, "catalog.json");
// Paths to pi's auth and model config files. The factory runs before pi binds
// its runtime APIs, so we read these directly when deciding whether to register
// gateway overrides. Use pi's own agent-dir resolver so PI_CODING_AGENT_DIR
// stays in sync.
const AUTH_FILE = join(getAgentDir(), "auth.json");
const MODELS_FILE = join(getAgentDir(), "models.json");

type GatewayProviderInfo = {
  config: ProviderConfig;
  // Model ids the gateway accepts for this provider. Unknown when falling back
  // to pi's built-in catalog, so custom models are treated conservatively.
  modelIds?: ReadonlySet<string>;
};

function readJSONFile(path: string): unknown | undefined {
  if (!existsSync(path)) return undefined;
  try {
    return JSON.parse(readFileSync(path, "utf8")) as unknown;
  } catch {
    return undefined;
  }
}

// loadAuthProviders returns ids with a non-empty entry in auth.json. pi removes
// entries on logout rather than leaving empty objects, so any present
// non-empty object means the user has set credentials. Deliberately uncached:
// /login can add credentials while pi is running.
function loadAuthProviders(): Set<string> {
  const out = new Set<string>();
  const parsed = readJSONFile(AUTH_FILE);
  if (!parsed || typeof parsed !== "object") return out;
  for (const [id, entry] of Object.entries(parsed as Record<string, unknown>)) {
    if (entry && typeof entry === "object" && Object.keys(entry as Record<string, unknown>).length > 0) {
      out.add(id);
    }
  }
  return out;
}

function hasNonEmptyObject(value: unknown): boolean {
  return (
    !!value &&
    typeof value === "object" &&
    !Array.isArray(value) &&
    Object.keys(value as Record<string, unknown>).length > 0
  );
}

function nonEmptyString(value: unknown): boolean {
  return typeof value === "string" && value.length > 0;
}

function modelNeedsDirectRoute(model: unknown, gatewayModelIds: ReadonlySet<string> | undefined): boolean {
  if (!model || typeof model !== "object" || Array.isArray(model)) return true;
  const cfg = model as Record<string, unknown>;
  if (nonEmptyString(cfg.baseUrl) || nonEmptyString(cfg.api) || hasNonEmptyObject(cfg.headers)) return true;
  if (!gatewayModelIds) return true;
  return typeof cfg.id !== "string" || !gatewayModelIds.has(cfg.id);
}

// models.json can contain pure metadata tweaks (compat/modelOverrides, or a
// model entry that only renames a gateway-supported id). Those should not
// disable the gateway. Only auth, endpoint/request settings, or custom models
// need pi's built-in/custom provider route to win.
function providerNeedsDirectRoute(entry: unknown, gatewayModelIds: ReadonlySet<string> | undefined): boolean {
  if (!entry || typeof entry !== "object" || Array.isArray(entry)) return false;
  const cfg = entry as Record<string, unknown>;
  if (
    nonEmptyString(cfg.apiKey) ||
    nonEmptyString(cfg.baseUrl) ||
    nonEmptyString(cfg.api) ||
    typeof cfg.authHeader === "boolean" ||
    hasNonEmptyObject(cfg.headers)
  ) {
    return true;
  }
  return Array.isArray(cfg.models) && cfg.models.some((model) => modelNeedsDirectRoute(model, gatewayModelIds));
}

function loadModelsJSONRoutingProviders(providerInfos: Map<string, GatewayProviderInfo>): Set<string> {
  const out = new Set<string>();
  const parsed = readJSONFile(MODELS_FILE);
  if (!parsed || typeof parsed !== "object") return out;
  const providers = (parsed as { providers?: unknown }).providers;
  if (!providers || typeof providers !== "object" || Array.isArray(providers)) return out;
  for (const [id, entry] of Object.entries(providers as Record<string, unknown>)) {
    const info = providerInfos.get(id);
    if (info && providerNeedsDirectRoute(entry, info.modelIds)) out.add(id);
  }
  return out;
}

function loadUserConfiguredProviders(
  providerInfos: Map<string, GatewayProviderInfo>,
  ctx?: ExtensionContext,
): Set<string> {
  const out = loadModelsJSONRoutingProviders(providerInfos);
  if (ctx) {
    for (const id of providerInfos.keys()) {
      if (ctx.modelRegistry.authStorage.hasAuth(id)) out.add(id);
    }
    return out;
  }
  for (const id of loadAuthProviders()) {
    if (providerInfos.has(id)) out.add(id);
  }
  for (const id of providerInfos.keys()) {
    if ((findEnvKeys(id)?.length ?? 0) > 0) out.add(id);
  }
  return out;
}

// Keep in lockstep with llmpricing.CatalogSchemaVersion. Any breaking change
// there requires shipping a new exe-dev pi extension that knows the new shape;
// until then the extension treats the catalog as unavailable and falls back.
const SCHEMA_VERSION = 1;

// In-VM fetches go to a link-local address with no DNS, so anything beyond a
// short budget is almost certainly stuck. The fetch is non-blocking (it only
// updates the on-disk cache for the next launch), so a tight timeout is fine.
const FETCH_TIMEOUT_MS = 1500;

// Allowlists for fields we forward into pi-ai. Forwarding an unknown value
// would silently misconfigure the model, so we drop it and let pi-ai keep its
// auto-detected default. Keep these in sync with @mariozechner/pi-ai's types.
// pi-ai's Api type is `KnownApi | (string & {})`, which permits any string
// for forward compatibility. We don't get static checking for this list, so
// keep it in sync with pi-ai's KnownApi by hand.
const KNOWN_APIS: ReadonlySet<string> = new Set([
  "openai-completions",
  "openai-responses",
  "openai-codex-responses",
  "azure-openai-responses",
  "anthropic-messages",
  "bedrock-converse-stream",
  "google-generative-ai",
  "google-vertex",
  "mistral-conversations",
]);
const KNOWN_THINKING_FORMATS: ReadonlySet<string> = new Set([
  "openai",
  "openrouter",
  "deepseek",
  "zai",
  "qwen",
  "qwen-chat-template",
]);
const KNOWN_MAX_TOKENS_FIELDS: ReadonlySet<string> = new Set(["max_tokens", "max_completion_tokens"]);
const KNOWN_CACHE_CONTROL_FORMATS: ReadonlySet<string> = new Set(["anthropic"]);

// pi-ai's Model.compat is a discriminated union keyed on the model's Api
// type, so generic reads/writes are not type-safe. We build a structural bag
// of fields we know about and cast at the boundary when assigning to
// ProviderModelConfig.compat. The cast is sound as long as the field types
// here remain a subset of pi-ai's compat shapes.
type CompatBag = {
  supportsDeveloperRole?: boolean;
  supportsReasoningEffort?: boolean;
  maxTokensField?: "max_tokens" | "max_completion_tokens";
  thinkingFormat?: "openai" | "openrouter" | "deepseek" | "zai" | "qwen" | "qwen-chat-template";
  cacheControlFormat?: "anthropic";
};

interface CatalogCompat {
  supportsDeveloperRole?: boolean;
  maxTokensField?: string;
  supportsReasoningEffort?: boolean;
  thinkingFormat?: string;
  cacheControlFormat?: string;
}

interface CatalogModel {
  id: string;
  name?: string;
  type?: string;
  reasoning?: boolean;
  input?: ("text" | "image")[];
  contextWindow?: number;
  maxTokens?: number;
  cost: { input: number; output: number; cacheRead: number; cacheWrite: number };
  compat?: CatalogCompat;
}

interface CatalogProvider {
  id: string;
  path: string;
  api?: string;
  models: CatalogModel[];
}

interface Catalog {
  schemaVersion: number;
  providers: CatalogProvider[];
}

// validCatalog is a structural check that rejects shapes we cannot safely
// register from. It deliberately does not validate every field; per-provider
// and per-model errors are caught later in the registration loop.
function validCatalog(cat: unknown): cat is Catalog {
  if (!cat || typeof cat !== "object") return false;
  const c = cat as Catalog;
  if (c.schemaVersion !== SCHEMA_VERSION) return false;
  if (!Array.isArray(c.providers) || c.providers.length === 0) return false;
  return true;
}

// loadCatalogSync returns the freshest usable catalog, or null if none. The
// on-disk cache wins over the bundled fallback so users pick up updates
// without waiting for a new image. Synchronous so providers are registered
// before the factory returns and pi flushes them.
//
// A bad cache file (corrupt JSON or unknown schema) is removed eagerly: the
// bundled fallback is always available, and removing the bad file means a
// chronically failing refresh doesn't leave us logging the same warning
// every launch.
function loadCatalogSync(): { catalog: Catalog; source: string } | null {
  for (const path of [CACHE_FILE, BUNDLED_CATALOG]) {
    if (!existsSync(path)) continue;
    let reason: string | undefined;
    try {
      const cat = JSON.parse(readFileSync(path, "utf8")) as unknown;
      if (validCatalog(cat)) return { catalog: cat, source: path };
      reason = "schemaVersion or shape mismatch";
    } catch (err) {
      reason = (err as Error).message;
    }
    console.warn(`[pi-exe-dev] ignoring ${path}: ${reason}`);
    if (path === CACHE_FILE) {
      try {
        unlinkSync(path);
      } catch {
        // best-effort cleanup; bundled fallback still works
      }
    }
  }
  return null;
}

// refreshCatalogAsync fetches the latest catalog and writes it to the cache
// file for use on the next pi launch. We deliberately do not re-register
// providers in this session: pi.registerProvider supports it, but races with
// in-flight model selection are not worth the risk for a once-per-launch
// freshness gain. Failures are logged once and never propagate.
async function refreshCatalogAsync(): Promise<void> {
  let text: string;
  try {
    const res = await fetch(CATALOG_URL, { signal: AbortSignal.timeout(FETCH_TIMEOUT_MS) });
    if (!res.ok) {
      console.warn(`[pi-exe-dev] catalog fetch returned HTTP ${res.status}`);
      return;
    }
    text = await res.text();
  } catch (err) {
    console.warn(`[pi-exe-dev] catalog fetch failed: ${(err as Error).message}`);
    return;
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch (err) {
    console.warn(`[pi-exe-dev] catalog fetch returned invalid JSON: ${(err as Error).message}`);
    return;
  }
  if (!validCatalog(parsed)) {
    console.warn(`[pi-exe-dev] catalog fetch returned unrecognized shape; skipping cache update`);
    return;
  }
  // Atomic write: tempfile + rename keeps concurrent pi launches from
  // observing a half-written cache file. The .tmp suffix is per-pid so two
  // refreshes can't clobber each other's tempfiles either.
  const tmp = `${CACHE_FILE}.${process.pid}.tmp`;
  try {
    mkdirSync(CACHE_DIR, { recursive: true });
    writeFileSync(tmp, text);
    renameSync(tmp, CACHE_FILE);
  } catch (err) {
    console.warn(`[pi-exe-dev] catalog cache write failed: ${(err as Error).message}`);
    try {
      unlinkSync(tmp);
    } catch {
      // tempfile may not exist if mkdir/write failed first
    }
  }
}

// sanitizeCompat copies only fields whose values pi-ai understands. Unknown
// values would misconfigure the model silently; dropping them lets pi-ai fall
// back to its URL-based auto-detection. Returns undefined if nothing is set.
function sanitizeCompat(c: CatalogCompat | undefined, providerId: string, modelId: string): CompatBag | undefined {
  if (!c) return undefined;
  const out: CompatBag = {};
  if (typeof c.supportsDeveloperRole === "boolean") out.supportsDeveloperRole = c.supportsDeveloperRole;
  if (typeof c.supportsReasoningEffort === "boolean") out.supportsReasoningEffort = c.supportsReasoningEffort;
  if (c.maxTokensField) {
    if (KNOWN_MAX_TOKENS_FIELDS.has(c.maxTokensField)) {
      out.maxTokensField = c.maxTokensField as CompatBag["maxTokensField"];
    } else {
      console.warn(`[pi-exe-dev] dropping unknown maxTokensField "${c.maxTokensField}" on ${providerId}/${modelId}`);
    }
  }
  if (c.thinkingFormat) {
    if (KNOWN_THINKING_FORMATS.has(c.thinkingFormat)) {
      out.thinkingFormat = c.thinkingFormat as CompatBag["thinkingFormat"];
    } else {
      console.warn(`[pi-exe-dev] dropping unknown thinkingFormat "${c.thinkingFormat}" on ${providerId}/${modelId}`);
    }
  }
  if (c.cacheControlFormat) {
    if (KNOWN_CACHE_CONTROL_FORMATS.has(c.cacheControlFormat)) {
      out.cacheControlFormat = c.cacheControlFormat as CompatBag["cacheControlFormat"];
    } else {
      console.warn(`[pi-exe-dev] dropping unknown cacheControlFormat "${c.cacheControlFormat}" on ${providerId}/${modelId}`);
    }
  }
  return Object.keys(out).length > 0 ? out : undefined;
}

// chatModelsWithFullMetadata picks chat models whose metadata is complete
// enough for pi to register directly. Models with partial metadata are
// dropped (and logged) rather than silently registered with bogus values.
// At the provider level, returning an empty list lets pi keep its built-in
// catalog for that provider — see registerOne.
function chatModelsWithFullMetadata(p: CatalogProvider): ProviderModelConfig[] {
  const out: ProviderModelConfig[] = [];
  const skipped: string[] = [];
  for (const m of p.models) {
    if (m.type && m.type !== "chat") continue; // skip embedding/reranker
    // Use explicit nullish/empty checks so a (hypothetical) zero-valued
    // contextWindow or maxTokens isn't misread as "missing", and so an
    // explicit empty input array is treated as missing rather than valid.
    if (m.name == null || m.contextWindow == null || m.maxTokens == null || !m.input?.length) {
      // Partial metadata is a Go-side bug (entry in allowedModels, no
      // gatewayMeta). Surface it instead of hiding the model silently.
      if (m.name || m.contextWindow != null || m.maxTokens != null || m.input?.length) {
        skipped.push(m.id);
      }
      continue;
    }
    const compat = sanitizeCompat(m.compat, p.id, m.id);
    out.push({
      id: m.id,
      name: m.name,
      reasoning: m.reasoning ?? false,
      input: m.input,
      contextWindow: m.contextWindow,
      maxTokens: m.maxTokens,
      cost: m.cost,
      ...(compat ? { compat: compat as ProviderModelConfig["compat"] } : {}),
    });
  }
  if (skipped.length > 0) {
    console.warn(`[pi-exe-dev] dropping ${p.id} models with incomplete metadata: ${skipped.join(", ")}`);
  }
  return out;
}

// configFromCatalogProvider builds the gateway provider config from the
// catalog. p.id must be one of pi-ai's KnownProvider names (anthropic, openai,
// fireworks, ...) because pi merges its built-in catalog by provider name; the
// Go-side providerCatalog in llmpricing/pricing.go is the source of truth for
// these IDs.
function configFromCatalogProvider(p: CatalogProvider): ProviderConfig {
  const path = p.path.replace(/^\/+/, "");
  const config: ProviderConfig = {
    baseUrl: `${GATEWAY}/${path}`,
    apiKey: "gateway",
  };
  if (p.api) {
    if (KNOWN_APIS.has(p.api)) {
      config.api = p.api as ProviderConfig["api"];
    } else {
      console.warn(`[pi-exe-dev] dropping unknown api "${p.api}" for provider ${p.id}; pi-ai will auto-detect`);
    }
  }
  const models = chatModelsWithFullMetadata(p);
  // Leaving config.models undefined tells pi to keep its built-in catalog for
  // this provider. anthropic and openai rely on this — the gateway does not
  // ship rich metadata for them, only cost.
  if (models.length > 0) config.models = models;
  return config;
}

const FALLBACK_PROVIDER_CONFIGS: Array<[string, ProviderConfig]> = [
  ["anthropic", { baseUrl: `${GATEWAY}/anthropic`, apiKey: "gateway" }],
  ["openai", { baseUrl: `${GATEWAY}/openai/v1`, apiKey: "gateway" }],
  ["fireworks", { baseUrl: `${GATEWAY}/fireworks/inference/v1`, apiKey: "gateway", api: "openai-completions" }],
];

function isGatewayBaseUrl(baseUrl: string | undefined): boolean {
  return baseUrl === GATEWAY || baseUrl?.startsWith(`${GATEWAY}/`) === true;
}

function providerHasGatewayModels(ctx: ExtensionContext, providerId: string): boolean {
  return ctx.modelRegistry.getAll().some((m) => m.provider === providerId && isGatewayBaseUrl(m.baseUrl));
}

type CurrentModel = NonNullable<ExtensionContext["model"]>;

// replacementForCurrentGatewayModel returns a non-gateway model to select
// after unregistering this extension's provider override. Usually the same
// model id exists in pi's built-in catalog. For gateway-only catalog entries
// (notably Fireworks), keep the model metadata but borrow the restored
// provider's upstream base URL/API/compat so the user's credentials go direct.
function replacementForCurrentGatewayModel(ctx: ExtensionContext, current: CurrentModel): CurrentModel | undefined {
  const same = ctx.modelRegistry.find(current.provider, current.id);
  if (same && !isGatewayBaseUrl(same.baseUrl)) return same;
  const template = ctx.modelRegistry.getAll().find((m) => m.provider === current.provider && !isGatewayBaseUrl(m.baseUrl));
  if (!template) return undefined;
  return { ...current, baseUrl: template.baseUrl, api: template.api, compat: template.compat };
}

export default function (pi: ExtensionAPI) {
  // Only activate on exe.dev VMs.
  if (!existsSync("/exe.dev")) return;

  const gatewayInfos = new Map<string, GatewayProviderInfo>();
  const loaded = loadCatalogSync();
  if (loaded) {
    for (const p of loaded.catalog.providers) {
      const modelIds = new Set(p.models.map((m) => m.id));
      gatewayInfos.set(p.id, {
        config: configFromCatalogProvider(p),
        modelIds: modelIds.size > 0 ? modelIds : undefined,
      });
    }
  } else {
    console.warn(`[pi-exe-dev] no usable catalog; falling back to pi's built-in models for anthropic/openai/fireworks`);
    for (const [id, config] of FALLBACK_PROVIDER_CONFIGS) gatewayInfos.set(id, { config });
  }

  const registerGateway = (id: string, info: GatewayProviderInfo): boolean => {
    try {
      pi.registerProvider(id, info.config);
      return true;
    } catch (err) {
      console.warn(`[pi-exe-dev] failed to register provider ${id}: ${(err as Error).message}`);
      return false;
    }
  };

  const userConfiguredAtLoad = loadUserConfiguredProviders(gatewayInfos);
  for (const [id, info] of gatewayInfos) {
    if (userConfiguredAtLoad.has(id)) continue;
    registerGateway(id, info);
  }

  const selectDirectReplacement = async (ctx: ExtensionContext, id: string): Promise<void> => {
    const current = ctx.model;
    if (current?.provider !== id || !isGatewayBaseUrl(current.baseUrl)) return;
    const replacement = replacementForCurrentGatewayModel(ctx, current);
    if (!replacement) {
      console.warn(`[pi-exe-dev] no non-gateway replacement for current model ${current.id}; pick a new model`);
      return;
    }
    try {
      const ok = await pi.setModel(replacement);
      if (!ok) {
        console.warn(`[pi-exe-dev] setModel(${replacement.id}) failed after unregister: no auth for ${replacement.provider}`);
      }
    } catch (err) {
      console.warn(`[pi-exe-dev] setModel(${replacement.id}) failed after unregister: ${(err as Error).message}`);
    }
  };

  // Reconcile factory-time decisions with credentials/config changed later via
  // /login, /logout, auth.json edits, models.json edits, or /reload. This is
  // intentionally small and per-provider: unregister when direct user config
  // should win; restore the gateway when that config disappears.
  let syncing = false;
  const sync = async (ctx: ExtensionContext): Promise<void> => {
    if (syncing) return;
    syncing = true;
    try {
      ctx.modelRegistry.authStorage.reload();
      const userConfigured = loadUserConfiguredProviders(gatewayInfos, ctx);
      let refreshedForRestore = false;
      for (const [id, info] of gatewayInfos) {
        const hasGateway = providerHasGatewayModels(ctx, id);
        const wantsGateway = !userConfigured.has(id);
        if (hasGateway && !wantsGateway) {
          pi.unregisterProvider(id);
          await selectDirectReplacement(ctx, id);
        } else if (!hasGateway && wantsGateway) {
          if (!refreshedForRestore) {
            ctx.modelRegistry.refresh();
            refreshedForRestore = true;
          }
          registerGateway(id, info);
        }
      }
    } finally {
      syncing = false;
    }
  };

  // /reload reruns the factory, but the model registry persists across reloads;
  // session_start is the first hook with ctx, so reconcile stale overrides there.
  pi.on("session_start", async (event, ctx) => {
    if (event.reason === "startup" || event.reason === "reload") await sync(ctx);
  });
  pi.on("input", async (_event, ctx) => sync(ctx));
  pi.on("model_select", async (_event, ctx) => sync(ctx));

  // Refresh the on-disk cache so the next pi launch starts from fresh data.
  void refreshCatalogAsync();

  // Inject exe.dev context into the system prompt.
  pi.on("before_agent_start", async (event) => {
    return {
      systemPrompt:
        event.systemPrompt +
        `

You are running inside an exe.dev VM, which provides HTTPS proxy, auth, email, and more. Docs index: https://exe.dev/docs.md

`,
    };
  });
}
