import assert from "node:assert/strict";
import test from "node:test";
import {
  discoverIntegrationCatalogs,
  integrationPromptAvailabilityLabel,
  providerInfosFromIntegrationCatalogs,
  type Catalog,
  type JSONFetcher,
} from "./integration_catalog.ts";

const reflectionURL = "https://reflection.int.exe.xyz/integrations";

function openAIGPTModel(mode: "managed" | "chatgpt") {
  return {
    id: "openai/gpt-5.5",
    name: "GPT 5.5",
    provider: "openai",
    native_id: "gpt-5.5",
    apis: mode === "managed" ? ["openai_responses", "openai_chat"] : ["openai_responses"],
    limits: { context_window: 200000, max_output_tokens: 32000 },
    architecture: { input_modalities: ["text"] },
    exe_dev: { mode },
  };
}

function anthropicModel(id: string, name: string) {
  return {
    id: `anthropic/${id}`,
    name,
    provider: "anthropic",
    native_id: id,
    apis: ["anthropic_messages"],
    limits: { context_window: 128000, max_output_tokens: 32000 },
    architecture: { input_modalities: ["text"] },
    exe_dev: { mode: "managed" },
  };
}

function xaiGrokModel() {
  return {
    id: "xai/grok-4.5",
    name: "Grok 4.5",
    provider: "xai",
    native_id: "grok-4.5",
    apis: ["openai_responses", "openai_chat"],
    limits: { context_window: 500000, max_output_tokens: 30000 },
    architecture: { input_modalities: ["text", "image"] },
    exe_dev: { mode: "managed" },
  };
}

function customModel(provider: string, nativeID: string, apis: string[]) {
  return {
    id: `${provider}/${nativeID}`,
    name: nativeID,
    provider,
    native_id: nativeID,
    apis,
    limits: { context_window: 100000, max_output_tokens: 8000 },
    architecture: { input_modalities: ["text"] },
    exe_dev: { mode: "byok" },
  };
}

test("discovers reflection integrations and model catalogs without serial catalog probing", async () => {
  let activeCatalogFetches = 0;
  let maxActiveCatalogFetches = 0;
  const fetched: string[] = [];
  const fetchJSON: JSONFetcher = async (url) => {
    fetched.push(url);
    if (url.endsWith("/integrations")) {
      return {
        integrations: [
          {
            type: "llm",
            name: "beta",
            help: "try https://beta-help.int.exe.xyz for models, not legacy https://beta.int.exe.cloud",
          },
          { type: "llm", name: "alpha" },
          { type: "reflection", name: "ignore-me" },
        ],
      };
    }
    if (url.endsWith("/models.json")) {
      activeCatalogFetches++;
      maxActiveCatalogFetches = Math.max(maxActiveCatalogFetches, activeCatalogFetches);
      await Promise.resolve();
      activeCatalogFetches--;
      return { schema_version: 1, models: [] };
    }
    return undefined;
  };

  const discovered = await discoverIntegrationCatalogs(reflectionURL, fetchJSON);

  assert.equal(discovered.found, true);
  assert.deepEqual(
    discovered.integrations.map((integration) => integration.name),
    ["beta", "alpha"],
  );
  assert.ok(fetched.includes("https://alpha.int.exe.xyz/models.json"));
  assert.ok(fetched.includes("https://beta-help.int.exe.xyz/models.json"));
  assert.equal(fetched.some((url) => url.includes(".exe.cloud")), false);
  assert.ok(maxActiveCatalogFetches > 1, `catalog fetches were serial; max active was ${maxActiveCatalogFetches}`);
});

test("keeps integration ownership when reflection succeeds but catalogs fail", async () => {
  const warnings: string[] = [];
  const fetchJSON: JSONFetcher = async (url) => {
    if (url.endsWith("/integrations")) return { integrations: [{ type: "llm", name: "broken" }] };
    throw new Error("offline");
  };

  const discovered = await discoverIntegrationCatalogs(reflectionURL, fetchJSON, (message) => warnings.push(message));

  assert.equal(discovered.found, true);
  assert.equal(discovered.integrations.length, 1);
  assert.equal(discovered.integrations[0]?.name, "broken");
  assert.equal(discovered.integrations[0]?.baseURL, "https://broken.int.exe.xyz");
  assert.equal(discovered.integrations[0]?.catalog, undefined);
  assert.equal(warnings.length, 1);
  assert.match(warnings[0] ?? "", /models\.json fetch failed/);
});

test("discovers team llm integrations through team hosts", async () => {
  const fetched: string[] = [];
  const fetchJSON: JSONFetcher = async (url) => {
    fetched.push(url);
    if (url.endsWith("/integrations")) {
      return {
        integrations: [
          { type: "llm", name: "shared", team: true },
          { type: "llm", name: "shared" },
        ],
      };
    }
    if (url === "https://shared.team.exe.xyz/models.json" || url === "https://shared.int.exe.xyz/models.json") {
      return { schema_version: 1, models: [] };
    }
    return undefined;
  };

  const discovered = await discoverIntegrationCatalogs(reflectionURL, fetchJSON);

  assert.equal(discovered.found, true);
  assert.deepEqual(
    discovered.integrations.map((integration) => integration.baseURL),
    ["https://shared.team.exe.xyz", "https://shared.int.exe.xyz"],
  );
  assert.ok(fetched.includes("https://shared.team.exe.xyz/models.json"));
});

test("returns no integrations when reflection has no attached llm", async () => {
  const discovered = await discoverIntegrationCatalogs(reflectionURL, async () => ({ integrations: [] }));

  assert.equal(discovered.found, false);
  assert.deepEqual(discovered.integrations, []);
});

test("namespaces reflected xAI models and never creates routes from pricing metadata", () => {
  const pricingCatalog: Catalog = {
    schemaVersion: 1,
    providers: [
      {
        id: "xai",
        path: "xai/v1",
        api: "openai-responses",
        models: [
          {
            id: "grok-4.5",
            name: "Grok 4.5",
            type: "chat",
            reasoning: true,
            input: ["text", "image"],
            contextWindow: 500000,
            maxTokens: 30000,
            cost: { input: 2, output: 6, cacheRead: 0.5, cacheWrite: 0 },
          },
        ],
      },
      {
        id: "fireworks",
        path: "fireworks/inference/v1",
        api: "openai-completions",
        models: [
          {
            id: "pricing-only-model",
            name: "Pricing Only",
            type: "chat",
            input: ["text"],
            contextWindow: 1000,
            maxTokens: 100,
            cost: { input: 1, output: 1, cacheRead: 0, cacheWrite: 0 },
          },
        ],
      },
    ],
  };

  const infos = providerInfosFromIntegrationCatalogs(
    [
      {
        name: "llm",
        baseURL: "https://llm.int.exe.xyz",
        catalog: { schema_version: 1, models: [xaiGrokModel()] },
      },
    ],
    pricingCatalog,
    (message) => assert.fail(`unexpected warning: ${message}`),
  );

  assert.deepEqual(Array.from(infos.keys()), ["exe-dev-xai"]);
  const xai = infos.get("exe-dev-xai");
  assert.ok(xai);
  assert.equal(xai.config.baseUrl, "https://llm.int.exe.xyz/xai/v1");
  assert.equal(xai.config.apiKey, "integration");
  assert.deepEqual(xai.config.models, [
    {
      id: "grok-4.5@llm",
      name: "Grok 4.5 (llm)",
      api: "openai-responses",
      baseUrl: "https://llm.int.exe.xyz/xai/v1",
      reasoning: true,
      input: ["text", "image"],
      contextWindow: 500000,
      maxTokens: 30000,
      cost: { input: 2, output: 6, cacheRead: 0.5, cacheWrite: 0 },
    },
  ]);
});

test("routes arbitrary providers by client protocol priority", () => {
  const infos = providerInfosFromIntegrationCatalogs(
    [
      {
        name: "llm",
        baseURL: "https://llm.int.exe.xyz",
        catalog: {
          schema_version: 1,
          models: [
            customModel("openrouter", "anthropic/claude-sonnet-4.6", ["openai_chat"]),
            customModel("multi-protocol", "multi-model", [
              "openai_chat",
              "anthropic_messages",
              "openai_responses",
            ]),
            customModel("anthropic-proxy", "claude-custom", ["openai_chat", "anthropic_messages"]),
            customModel("embedding-only", "embedding-model", ["openai_embeddings"]),
          ],
        },
      },
    ],
    undefined,
    () => {},
  );

  assert.deepEqual(Array.from(infos.keys()).sort(), [
    "exe-dev-anthropic-proxy",
    "exe-dev-multi-protocol",
    "exe-dev-openrouter",
  ]);

  const openrouter = infos.get("exe-dev-openrouter");
  assert.ok(openrouter);
  assert.deepEqual(openrouter.config.models, [
    {
      id: "anthropic/claude-sonnet-4.6@llm",
      name: "anthropic/claude-sonnet-4.6 (llm)",
      api: "openai-completions",
      baseUrl: "https://llm.int.exe.xyz/openrouter/v1",
      reasoning: false,
      input: ["text"],
      contextWindow: 100000,
      maxTokens: 8000,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
    },
  ]);
  assert.equal(
    openrouter.modelAliases?.get("anthropic/claude-sonnet-4.6@llm"),
    "anthropic/claude-sonnet-4.6",
  );

  const multi = infos.get("exe-dev-multi-protocol");
  assert.ok(multi);
  assert.equal(multi.config.models?.[0]?.api, "openai-responses");
  assert.equal(multi.config.models?.[0]?.baseUrl, "https://llm.int.exe.xyz/multi-protocol/v1");

  const anthropicProxy = infos.get("exe-dev-anthropic-proxy");
  assert.ok(anthropicProxy);
  assert.equal(anthropicProxy.config.models?.[0]?.api, "anthropic-messages");
  assert.equal(anthropicProxy.config.models?.[0]?.baseUrl, "https://llm.int.exe.xyz/anthropic-proxy");
});

test("preserves duplicate model names and marks ChatGPT rewrites by generated model id", () => {
  const pricingCatalog: Catalog = {
    schemaVersion: 1,
    providers: [
      {
        id: "openai",
        path: "openai/v1",
        models: [
          {
            id: "gpt-5.5",
            name: "GPT 5.5",
            type: "chat",
            input: ["text"],
            contextWindow: 200000,
            maxTokens: 32000,
            cost: { input: 1, output: 2, cacheRead: 0.1, cacheWrite: 0.2 },
          },
        ],
      },
    ],
  };
  const infos = providerInfosFromIntegrationCatalogs(
    [
      {
        name: "chatgpt-sub",
        baseURL: "https://chatgpt-sub.int.exe.xyz",
        catalog: { schema_version: 1, models: [openAIGPTModel("chatgpt")] },
      },
      {
        name: "managed-sub",
        baseURL: "https://managed-sub.int.exe.xyz",
        catalog: { schema_version: 1, models: [openAIGPTModel("managed")] },
      },
    ],
    pricingCatalog,
    (message) => assert.fail(`unexpected warning: ${message}`),
  );

  const openai = infos.get("exe-dev-openai");
  assert.ok(openai);
  assert.equal(openai.config.name, "chatgpt-sub, managed-sub");
  assert.deepEqual(
    openai.config.models?.map((model) => model.id),
    ["gpt-5.5@chatgpt-sub", "gpt-5.5@managed-sub"],
  );
  assert.equal(openai.modelAliases?.get("gpt-5.5@chatgpt-sub"), "gpt-5.5");
  assert.equal(openai.modelAliases?.get("gpt-5.5@managed-sub"), "gpt-5.5");
  assert.equal(openai.chatGPTModelIds?.has("gpt-5.5@chatgpt-sub"), true);
  assert.equal(openai.chatGPTModelIds?.has("gpt-5.5"), false);
  assert.deepEqual(
    openai.config.models?.map((model) => model.baseUrl),
    ["https://chatgpt-sub.int.exe.xyz/openai/v1", "https://managed-sub.int.exe.xyz/openai/v1"],
  );
});

test("preserves model order when native ids repeat across integrations", () => {
  const infos = providerInfosFromIntegrationCatalogs(
    [
      {
        name: "first",
        baseURL: "https://first.int.exe.xyz",
        catalog: {
          schema_version: 1,
          models: [
            customModel("openrouter", "duplicate", ["openai_chat"]),
            customModel("openrouter", "between", ["openai_chat"]),
          ],
        },
      },
      {
        name: "second",
        baseURL: "https://second.int.exe.xyz",
        catalog: {
          schema_version: 1,
          models: [customModel("openrouter", "duplicate", ["openai_chat"])],
        },
      },
    ],
    undefined,
    () => {},
  );

  assert.deepEqual(
    infos.get("exe-dev-openrouter")?.config.models?.map((model) => model.id),
    ["duplicate@first", "between@first", "duplicate@second"],
  );
});

test("preserves reflected catalog model order for provider configs", () => {
  const pricingCatalog: Catalog = {
    schemaVersion: 1,
    providers: [
      {
        id: "anthropic",
        path: "anthropic",
        models: [
          {
            id: "claude-opus-4-8",
            name: "Claude Opus 4.8",
            type: "chat",
            input: ["text"],
            contextWindow: 128000,
            maxTokens: 32000,
            cost: { input: 1, output: 2, cacheRead: 0.1, cacheWrite: 0.2 },
          },
          {
            id: "claude-fable-5",
            name: "Claude Fable 5",
            type: "chat",
            input: ["text"],
            contextWindow: 128000,
            maxTokens: 32000,
            cost: { input: 1, output: 2, cacheRead: 0.1, cacheWrite: 0.2 },
          },
        ],
      },
    ],
  };
  const infos = providerInfosFromIntegrationCatalogs(
    [
      {
        name: "llm",
        baseURL: "https://llm.int.exe.xyz",
        catalog: {
          schema_version: 1,
          models: [
            anthropicModel("claude-opus-4-8", "Claude Opus 4.8"),
            anthropicModel("claude-fable-5", "Claude Fable 5"),
          ],
        },
      },
    ],
    pricingCatalog,
    (message) => assert.fail(`unexpected warning: ${message}`),
  );

  const anthropic = infos.get("exe-dev-anthropic");
  assert.ok(anthropic);
  assert.equal(anthropic.config.baseUrl, "https://llm.int.exe.xyz/anthropic");
  assert.deepEqual(
    anthropic.config.models?.map((model) => model.id),
    ["claude-opus-4-8@llm", "claude-fable-5@llm"],
  );
});

test("warns once when pricing is absent for multiple integration models", () => {
  const warnings: string[] = [];
  const infos = providerInfosFromIntegrationCatalogs(
    [
      {
        name: "one",
        baseURL: "https://one.int.exe.xyz",
        catalog: {
          schema_version: 1,
          models: [openAIGPTModel("managed"), anthropicModel("claude-fable-5", "Claude Fable 5")],
        },
      },
      {
        name: "two",
        baseURL: "https://two.int.exe.xyz",
        catalog: { schema_version: 1, models: [openAIGPTModel("managed")] },
      },
    ],
    undefined,
    (message) => warnings.push(message),
  );

  assert.ok(infos.get("exe-dev-openai"));
  assert.ok(infos.get("exe-dev-anthropic"));
  assert.equal(warnings.length, 1);
  assert.match(warnings[0] ?? "", /missing pricing for 2 integration models/);
});

test("formats prompt integration availability with at most two names", () => {
  assert.equal(integrationPromptAvailabilityLabel(["beta", "alpha"]), "alpha, beta");
  assert.equal(integrationPromptAvailabilityLabel(["beta", "alpha", "gamma"]), "alpha, beta, ...");
  assert.equal(integrationPromptAvailabilityLabel(["beta", "alpha", "beta", "gamma"]), "alpha, beta, ...");
});
