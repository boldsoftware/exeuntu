import type { ProviderConfig, ProviderModelConfig } from "@mariozechner/pi-coding-agent";

export type GatewayProviderInfo = {
  config: ProviderConfig;
  // Model ids the gateway accepts for this provider. Unknown when falling back
  // to pi's built-in catalog, so custom models are treated conservatively.
  modelIds?: ReadonlySet<string>;
};

// Keep in lockstep with llmpricing.CatalogSchemaVersion. Any breaking change
// there requires shipping a new exe-dev pi extension that knows the new shape;
// until then the extension treats the catalog as unavailable and falls back.
export const SCHEMA_VERSION = 1;

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

export interface CatalogCompat {
  supportsDeveloperRole?: boolean;
  maxTokensField?: string;
  supportsReasoningEffort?: boolean;
  thinkingFormat?: string;
  cacheControlFormat?: string;
}

export interface CatalogModel {
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

export interface CatalogProvider {
  id: string;
  path: string;
  api?: string;
  models: CatalogModel[];
}

export interface Catalog {
  schemaVersion: number;
  providers: CatalogProvider[];
}

// validCatalog is a structural check that rejects shapes we cannot safely
// register from. It deliberately does not validate every field; per-provider
// and per-model errors are caught later in the registration loop.
export function validCatalog(cat: unknown): cat is Catalog {
  if (!cat || typeof cat !== "object") return false;
  const c = cat as Catalog;
  if (c.schemaVersion !== SCHEMA_VERSION) return false;
  if (!Array.isArray(c.providers) || c.providers.length === 0) return false;
  return true;
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
export function configFromCatalogProvider(p: CatalogProvider, gatewayBaseURL: string): ProviderConfig {
  const path = p.path.replace(/^\/+/, "");
  const config: ProviderConfig = {
    baseUrl: `${gatewayBaseURL}/${path}`,
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
