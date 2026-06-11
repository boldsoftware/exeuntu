import {
  getAgentDir,
  type ExtensionAPI,
  type ExtensionContext,
  type ProviderConfig,
} from "@mariozechner/pi-coding-agent";
import { findEnvKeys } from "@mariozechner/pi-ai";
import { existsSync, mkdirSync, readFileSync, renameSync, unlinkSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import {
  configFromCatalogProvider,
  validCatalog,
  type Catalog,
  type GatewayProviderInfo,
} from "./integration_catalog.ts";

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
// Per-user record of the last routing fingerprint surfaced via ctx.ui.notify,
// used to suppress repeat notifications when nothing has changed across pi
// launches. AGENTS.md asks us to be sparing with text in the SSH UI.
const NOTIFY_STATE_FILE = join(CACHE_DIR, "last-routing.json");

// Paths to pi's auth and model config files. The factory runs before pi binds
// its runtime APIs, so we read these directly when deciding whether to register
// gateway overrides. Use pi's own agent-dir resolver so PI_CODING_AGENT_DIR
// stays in sync.
const AUTH_FILE = join(getAgentDir(), "auth.json");
const MODELS_FILE = join(getAgentDir(), "models.json");

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

// In-VM fetches go to a link-local address with no DNS, so anything beyond a
// short budget is almost certainly stuck. The fetch is non-blocking (it only
// updates the on-disk cache for the next launch), so a tight timeout is fine.
const FETCH_TIMEOUT_MS = 1500;

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

let procEnvCache: Map<string, string> | null = null;

// envValue mirrors pi-ai's Bun compiled-binary workaround for sandboxed Linux
// environments where process.env can be empty. Local to the exe.dev kill
// switch; provider env vars use pi-ai's findEnvKeys().
function envValue(key: string): string | undefined {
  const value = process.env[key];
  if (value !== undefined) return value;
  if (!process.versions?.bun || Object.keys(process.env).length > 0) return undefined;
  if (procEnvCache === null) {
    procEnvCache = new Map();
    try {
      const data = readFileSync("/proc/self/environ", "utf8");
      for (const entry of data.split("\0")) {
        const idx = entry.indexOf("=");
        if (idx > 0) procEnvCache.set(entry.slice(0, idx), entry.slice(idx + 1));
      }
    } catch {
      // /proc/self/environ may not be readable.
    }
  }
  return procEnvCache.get(key);
}

// Master kill-switch. Setting EXE_DEV_DISABLE_GATEWAY to a truthy value
// ("1", "true", "yes", or "on", case-insensitive) makes the extension skip
// every gateway provider registration. The exe.dev system-prompt injection
// still runs so the model knows it's in a VM, but pi falls back to its
// built-in providers and the user's own credentials.
//
// Allowlisting truthy values rather than blocklisting falsy ones avoids the
// systemd-style trap where EXE_DEV_DISABLE_GATEWAY=off would otherwise
// silently *disable* the gateway. Read once when the extension factory runs;
// /reload reruns the factory and picks up changes.
const TRUTHY_KILL_SWITCH = new Set(["1", "true", "yes", "on"]);
function gatewayDisabled(): boolean {
  const v = envValue("EXE_DEV_DISABLE_GATEWAY");
  if (v == null) return false;
  return TRUTHY_KILL_SWITCH.has(v.toLowerCase());
}

export default function (pi: ExtensionAPI) {
  // Only activate on exe.dev VMs.
  if (!existsSync("/exe.dev")) return;

  const disabled = gatewayDisabled();

  const gatewayInfos = new Map<string, GatewayProviderInfo>();
  const loaded = loadCatalogSync();
  if (loaded) {
    for (const p of loaded.catalog.providers) {
      const modelIds = new Set(p.models.map((m) => m.id));
      gatewayInfos.set(p.id, {
        config: configFromCatalogProvider(p, GATEWAY),
        modelIds: modelIds.size > 0 ? modelIds : undefined,
      });
    }
  } else {
    if (!disabled) {
      console.warn(`[pi-exe-dev] no usable catalog; falling back to pi's built-in models for anthropic/openai/fireworks`);
    }
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
  if (!disabled) {
    for (const [id, info] of gatewayInfos) {
      if (userConfiguredAtLoad.has(id)) continue;
      registerGateway(id, info);
    }
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
        const wantsGateway = !disabled && !userConfigured.has(id);
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

  const routingForNotice = (ctx: ExtensionContext): { gateway: string[]; userConfig: string[] } => {
    const userConfigured = loadUserConfiguredProviders(gatewayInfos, ctx);
    const gateway: string[] = [];
    const userConfig: string[] = [];
    for (const id of gatewayInfos.keys()) {
      if (providerHasGatewayModels(ctx, id)) gateway.push(id);
      else if (userConfigured.has(id)) userConfig.push(id);
    }
    return { gateway: gateway.sort(), userConfig: userConfig.sort() };
  };

  // Surface routing once on startup/reload. Stay quiet in the common all-gateway
  // path, but notify when any gateway provider is bypassed by user config or
  // when the kill switch is active. Startup notifications are de-duped across
  // launches; /reload always shows the current decision.
  pi.on("session_start", async (event, ctx) => {
    if (event.reason !== "startup" && event.reason !== "reload") return;
    await sync(ctx);
    if (!ctx.hasUI) return;

    const routing = routingForNotice(ctx);
    let message: string | undefined;
    if (disabled) {
      message = "exe.dev gateway disabled (EXE_DEV_DISABLE_GATEWAY); using your own provider credentials.";
    } else if (routing.userConfig.length > 0) {
      const parts = [`Using your credentials/config for: ${routing.userConfig.join(", ")}.`];
      if (routing.gateway.length > 0) {
        parts.push(`Using exe.dev gateway for: ${routing.gateway.join(", ")}.`);
        parts.push("Set EXE_DEV_DISABLE_GATEWAY=1 to bypass the gateway entirely.");
      }
      message = parts.join(" ");
    }
    if (message == null) {
      try {
        unlinkSync(NOTIFY_STATE_FILE);
      } catch {
        // Already absent or unreadable; no-op.
      }
      return;
    }

    const fingerprint = JSON.stringify({
      v: 1,
      disabled,
      gateway: routing.gateway,
      userCreds: routing.userConfig,
    });
    if (event.reason === "startup") {
      try {
        if (readFileSync(NOTIFY_STATE_FILE, "utf8") === fingerprint) return;
      } catch {
        // First run, or unreadable: notify.
      }
    }
    ctx.ui.notify(message, "info");
    try {
      mkdirSync(CACHE_DIR, { recursive: true });
      writeFileSync(NOTIFY_STATE_FILE, fingerprint);
    } catch {
      // Best effort: a write failure just means we'll re-show next launch.
    }
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
