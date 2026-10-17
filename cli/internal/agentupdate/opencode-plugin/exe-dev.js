import { existsSync } from "node:fs";

const REFLECTION_INTEGRATIONS_URL = "https://reflection.int.exe.xyz/integrations";
const FETCH_TIMEOUT_MS = 5000;
const TRUTHY = new Set(["1", "true", "yes", "on"]);

// OpenCode 2 bundles these provider runtimes; nothing is fetched from npm.
const ADAPTERS = {
  responses: {
    package: "@opencode/ai/providers/openai/responses",
    label: "OpenAI Responses",
  },
  messages: {
    package: "@opencode/ai/providers/anthropic",
    label: "Anthropic Messages",
  },
  chat: {
    package: "@opencode/ai/providers/openai-compatible",
    label: "OpenAI Chat",
  },
};
const MODALITIES = ["text", "audio", "image", "video", "pdf"];

function integrationsDisabled() {
  return TRUTHY.has((process.env.EXE_DEV_DISABLE_GATEWAY || "").toLowerCase());
}

function runningOnExeDev() {
  if (process.env.EXE_DEV_PLUGIN_TEST !== undefined) {
    return process.env.EXE_DEV_PLUGIN_TEST === "1";
  }
  return existsSync("/exe.dev");
}

// Fetch with a single retry on network failure.
async function fetchWithRetry(url) {
  try {
    return await fetch(url, { signal: AbortSignal.timeout(FETCH_TIMEOUT_MS) });
  } catch {
    return await fetch(url, { signal: AbortSignal.timeout(FETCH_TIMEOUT_MS) });
  }
}

async function fetchJSON(url) {
  const response = await fetchWithRetry(url);
  if (!response.ok) throw new Error(`${url}: HTTP ${response.status}`);
  return response.json();
}

function reflectionIntegrations(value) {
  if (!value || typeof value !== "object" || !Array.isArray(value.integrations)) {
    throw new Error(`${REFLECTION_INTEGRATIONS_URL} returned unrecognized reflection shape`);
  }
  return value.integrations.filter(
    (integration) =>
      integration?.type === "llm" &&
      typeof integration.name === "string" &&
      integration.name.trim().length > 0,
  );
}

function validCatalog(value) {
  return (
    value &&
    typeof value === "object" &&
    value.schema_version === 1 &&
    Array.isArray(value.models)
  );
}

function integrationBaseURL(integration) {
  const name = integration.name.trim();
  return integration.team === true
    ? `https://${name}.team.exe.xyz`
    : `https://${name}.int.exe.xyz`;
}

function integrationBaseURLCandidates(integration) {
  const candidates = [];
  const add = (value) => {
    if (!candidates.includes(value)) candidates.push(value);
  };
  const help = typeof integration.help === "string" ? integration.help : "";
  for (const match of help.matchAll(/https?:\/\/[^\s'"`]+/g)) {
    try {
      const url = new URL(match[0]);
      if (
        url.hostname.endsWith(".int.exe.xyz") ||
        url.hostname.endsWith(".team.exe.xyz")
      ) {
        add(url.origin);
      }
    } catch {
      // Ignore prose that only looked like a URL.
    }
  }
  add(integrationBaseURL(integration));
  return candidates;
}

async function fetchIntegration(integration) {
  const name = integration.name.trim();
  const candidates = integrationBaseURLCandidates(integration);
  try {
    return await Promise.any(
      candidates.map(async (baseURL) => {
        const catalog = await fetchJSON(`${baseURL}/models.json`);
        if (!validCatalog(catalog)) {
          throw new Error(`${baseURL}/models.json returned unrecognized catalog shape`);
        }
        return {
          name,
          team: integration.team === true,
          baseURL,
          models: catalog.models,
        };
      }),
    );
  } catch (error) {
    const detail = error instanceof AggregateError && error.errors.length > 0
      ? error.errors[0]?.message
      : error?.message;
    console.warn(`[opencode-exe-dev] LLM integration ${name} models.json fetch failed: ${detail || error}`);
    return undefined;
  }
}

function adapterFor(model) {
  const apis = new Set(Array.isArray(model.apis) ? model.apis : []);
  // Preserve Anthropic-specific thinking/cache transforms when available.
  if (model.provider === "anthropic" && apis.has("anthropic_messages")) return "messages";
  if (apis.has("openai_responses")) return "responses";
  if (apis.has("anthropic_messages")) return "messages";
  if (apis.has("openai_chat")) return "chat";
  return undefined;
}

function validCatalogProvider(provider) {
  return typeof provider === "string" && provider !== "accounts" && /^[a-z0-9_-]+$/.test(provider);
}

function stableSlug(name) {
  let out = "";
  for (const char of name.toLowerCase()) {
    if (/^[a-z0-9_-]$/.test(char)) out += char;
    else out += `_${char.codePointAt(0).toString(16)}_`;
  }
  return out || "integration";
}

function integrationRouteName(integration) {
  try {
    const hostname = new URL(integration.baseURL).hostname;
    return hostname
      .replace(/\.team\.exe\.xyz$/, "")
      .replace(/\.int\.exe\.xyz$/, "");
  } catch {
    return integration.name;
  }
}

function providerIDComponent(value) {
  return `${value.length}-${value}`;
}

function baseProviderID(integration, provider, adapter) {
  const scope = integration.team ? "t" : "p";
  const name = stableSlug(integration.name);
  const route = stableSlug(integrationRouteName(integration));
  return [
    "exe",
    scope,
    providerIDComponent(name),
    providerIDComponent(route),
    providerIDComponent(stableSlug(provider)),
    adapter,
  ].join("-");
}

function providerDisplayName(integration, provider, adapter) {
  const scope = integration.team ? `team ${integration.name}` : integration.name;
  return `exe.dev ${scope} · ${provider} · ${ADAPTERS[adapter].label}`;
}

function positiveNumber(value) {
  return typeof value === "number" && Number.isFinite(value) && value > 0
    ? value
    : undefined;
}

function modalities(value) {
  return Array.isArray(value) ? value.filter((item) => MODALITIES.includes(item)) : [];
}

// modelInfo returns an OpenCode 2 Model.Info. id is the OpenCode model key;
// modelID is what the gateway receives.
function modelInfo(providerID, model) {
  if (typeof model.id !== "string" || model.id.length === 0) return undefined;
  const requestID = typeof model.native_id === "string" && model.native_id.length > 0
    ? model.native_id
    : model.id;
  const input = modalities(model.architecture?.input_modalities);
  const output = modalities(model.architecture?.output_modalities);
  const context = positiveNumber(model.limits?.context_window) ?? 128000;
  return {
    id: modelKey(model),
    modelID: requestID,
    providerID,
    name: typeof model.name === "string" && model.name.length > 0 ? model.name : model.id,
    capabilities: {
      tools: Array.isArray(model.supported_parameters) && model.supported_parameters.includes("tools"),
      input: input.length > 0 ? input : ["text"],
      output: output.length > 0 ? output : ["text"],
    },
    variants: [],
    time: { released: 0 },
    cost: [],
    status: "active",
    enabled: true,
    limit: {
      context,
      output: positiveNumber(model.limits?.max_output_tokens) ?? Math.min(context, 4096),
    },
  };
}

function modelKey(model) {
  const prefix = `${model.provider}/`;
  return model.id.startsWith(prefix) ? model.id.slice(prefix.length) : model.id;
}

// integrationProviders converts discovered catalogs into OpenCode 2 provider
// records, one per (integration, upstream provider, wire protocol).
function integrationProviders(integrations) {
  const records = [];
  let omittedChatGPT = false;
  const ordered = [...integrations].sort((a, b) => {
    const left = `${a.team ? "1" : "0"}\0${a.name}\0${a.baseURL}`;
    const right = `${b.team ? "1" : "0"}\0${b.name}\0${b.baseURL}`;
    return left.localeCompare(right);
  });

  for (const integration of ordered) {
    const groups = new Map();
    for (const model of integration.models) {
      if (!model || typeof model !== "object" || !validCatalogProvider(model.provider)) continue;
      if (model.exe_dev?.mode === "chatgpt") {
        omittedChatGPT = true;
        continue;
      }
      const adapter = adapterFor(model);
      if (!adapter) continue;
      const providerID = baseProviderID(integration, model.provider, adapter);
      const info = modelInfo(providerID, model);
      if (!info) continue;
      let group = groups.get(providerID);
      if (!group) {
        group = {
          info: {
            id: providerID,
            name: providerDisplayName(integration, model.provider, adapter),
            activation: "enabled",
            package: ADAPTERS[adapter].package,
            settings: {
              baseURL: `${integration.baseURL.replace(/\/+$/, "")}/${encodeURIComponent(model.provider)}/v1`,
              apiKey: "implicit",
            },
          },
          models: new Map(),
        };
        groups.set(providerID, group);
      }
      group.models.set(info.id, info);
    }
    for (const group of groups.values()) {
      records.push({ info: group.info, models: [...group.models.values()] });
    }
  }

  if (omittedChatGPT) {
    console.warn("[opencode-exe-dev] ChatGPT-account models are not yet supported by the OpenCode integration");
  }
  return records;
}

async function discoverProviders() {
  let integrations;
  try {
    integrations = reflectionIntegrations(await fetchJSON(REFLECTION_INTEGRATIONS_URL));
  } catch (error) {
    console.warn(`[opencode-exe-dev] LLM integration reflection fetch failed: ${error?.message || error}`);
    return [];
  }
  const discovered = (await Promise.all(integrations.map(fetchIntegration))).filter(Boolean);
  return integrationProviders(discovered);
}

// OpenCode 2 plugin. Plugin.define from @opencode/plugin is the identity
// function, so a plain object avoids a runtime dependency.
export default {
  id: "exe.dev",
  async setup(ctx) {
    if (!runningOnExeDev() || integrationsDisabled()) return;
    const providers = await discoverProviders();
    if (providers.length === 0) return;
    await ctx.provider.transform((editor) => {
      // User config with the same provider ID overlays these definitions.
      for (const provider of providers) editor.add(provider);
    });
  },
};
