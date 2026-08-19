import { existsSync } from "node:fs";

const REFLECTION_INTEGRATIONS_URL = "https://reflection.int.exe.xyz/integrations";
const FETCH_TIMEOUT_MS = 1500;
const TRUTHY = new Set(["1", "true", "yes", "on"]);

const ADAPTERS = {
  responses: {
    npm: "@ai-sdk/openai",
    label: "OpenAI Responses",
  },
  messages: {
    npm: "@ai-sdk/anthropic",
    label: "Anthropic Messages",
  },
  chat: {
    npm: "@ai-sdk/openai-compatible",
    label: "OpenAI Chat",
  },
};

function integrationsDisabled() {
  return TRUTHY.has((process.env.EXE_DEV_DISABLE_GATEWAY || "").toLowerCase());
}

function runningOnExeDev() {
  if (process.env.EXE_DEV_PLUGIN_TEST !== undefined) {
    return process.env.EXE_DEV_PLUGIN_TEST === "1";
  }
  return existsSync("/exe.dev");
}

async function fetchJSON(url) {
  const response = await fetch(url, { signal: AbortSignal.timeout(FETCH_TIMEOUT_MS) });
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

function modelConfig(model) {
  const requestID = typeof model.native_id === "string" && model.native_id.length > 0
    ? model.native_id
    : model.id;
  if (typeof model.id !== "string" || model.id.length === 0 || typeof requestID !== "string" || requestID.length === 0) {
    return undefined;
  }

  const out = {
    id: requestID,
    name: typeof model.name === "string" && model.name.length > 0 ? model.name : model.id,
  };
  const input = Array.isArray(model.architecture?.input_modalities)
    ? model.architecture.input_modalities.filter((item) => ["text", "audio", "image", "video", "pdf"].includes(item))
    : [];
  const output = Array.isArray(model.architecture?.output_modalities)
    ? model.architecture.output_modalities.filter((item) => ["text", "audio", "image", "video", "pdf"].includes(item))
    : [];
  if (input.length > 0 || output.length > 0) {
    out.modalities = {};
    if (input.length > 0) out.modalities.input = input;
    if (output.length > 0) out.modalities.output = output;
  }
  if (input.includes("image") || input.includes("pdf")) out.attachment = true;
  if (Array.isArray(model.supported_parameters) && model.supported_parameters.includes("tools")) {
    out.tool_call = true;
  }

  const context = positiveNumber(model.limits?.context_window) ?? 128000;
  const outputLimit = positiveNumber(model.limits?.max_output_tokens) ?? Math.min(context, 4096);
  out.limit = { context, output: outputLimit };
  return out;
}

function modelKey(model) {
  const prefix = `${model.provider}/`;
  return model.id.startsWith(prefix) ? model.id.slice(prefix.length) : model.id;
}

function injectIntegrationProviders(config, integrations) {
  const injected = {};
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
      const entry = modelConfig(model);
      if (!entry) continue;
      const groupKey = `${model.provider}\0${adapter}`;
      let group = groups.get(groupKey);
      if (!group) {
        group = { provider: model.provider, adapter, models: {} };
        groups.set(groupKey, group);
      }
      group.models[modelKey(model)] = entry;
    }

    for (const { provider, adapter, models } of groups.values()) {
      const providerID = baseProviderID(integration, provider, adapter);
      injected[providerID] = {
        npm: ADAPTERS[adapter].npm,
        name: providerDisplayName(integration, provider, adapter),
        options: {
          baseURL: `${integration.baseURL.replace(/\/+$/, "")}/${encodeURIComponent(provider)}/v1`,
          apiKey: "implicit",
        },
        models,
      };
    }
  }

  // Copy-on-write prevents injected providers from leaking into the user's
  // opencode.json during a UI read-modify-write. Explicit user providers win.
  config.provider = { ...injected, ...(config.provider ?? {}) };

  if (omittedChatGPT) {
    console.warn("[opencode-exe-dev] ChatGPT-account models are not yet supported by the OpenCode integration");
  }
}

export default async function exeDevPlugin() {
  return {
    config: async (config) => {
      if (!runningOnExeDev() || integrationsDisabled()) return;

      let integrations;
      try {
        integrations = reflectionIntegrations(await fetchJSON(REFLECTION_INTEGRATIONS_URL));
      } catch (error) {
        console.warn(`[opencode-exe-dev] LLM integration reflection fetch failed: ${error?.message || error}`);
        return;
      }
      const discovered = (await Promise.all(integrations.map(fetchIntegration))).filter(Boolean);
      if (discovered.length === 0) return;
      injectIntegrationProviders(config, discovered);
    },
  };
}
