import assert from "node:assert/strict";
import test from "node:test";
import {
  discoverIntegrationCatalogs,
  integrationPromptAvailabilityLabel,
  providerInfosFromIntegrationCatalogs,
  type Catalog,
  type JSONFetcher,
} from "./integration_catalog.ts";

const reflectionURLs = [
  "https://reflection.int.exe.xyz/integrations",
  "https://reflection.int.exe.cloud/integrations",
  "http://reflection.int.exe.cloud/integrations",
];

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

test("discovers reflection integrations and model catalogs without serial catalog probing", async () => {
  let activeCatalogFetches = 0;
  let maxActiveCatalogFetches = 0;
  const fetched: string[] = [];
  const fetchJSON: JSONFetcher = async (url) => {
    fetched.push(url);
    if (url.endsWith("/integrations")) {
      return {
        integrations: [
          { type: "llm", name: "beta", help: "try https://beta-help.int.exe.xyz for models" },
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

  const discovered = await discoverIntegrationCatalogs(reflectionURLs, fetchJSON);

  assert.equal(discovered.found, true);
  assert.deepEqual(
    discovered.integrations.map((integration) => integration.name),
    ["alpha", "beta"],
  );
  assert.ok(fetched.includes("https://alpha.int.exe.xyz/models.json"));
  assert.ok(fetched.includes("https://beta-help.int.exe.xyz/models.json"));
  assert.ok(maxActiveCatalogFetches > 1, `catalog fetches were serial; max active was ${maxActiveCatalogFetches}`);
});

test("keeps integration ownership when reflection succeeds but catalogs fail", async () => {
  const warnings: string[] = [];
  const fetchJSON: JSONFetcher = async (url) => {
    if (url.endsWith("/integrations")) return { integrations: [{ type: "llm", name: "broken" }] };
    throw new Error("offline");
  };

  const discovered = await discoverIntegrationCatalogs(reflectionURLs, fetchJSON, (message) => warnings.push(message));

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

  const discovered = await discoverIntegrationCatalogs(reflectionURLs, fetchJSON);

  assert.equal(discovered.found, true);
  assert.deepEqual(
    discovered.integrations.map((integration) => integration.baseURL).sort(),
    ["https://shared.int.exe.xyz", "https://shared.team.exe.xyz"],
  );
  assert.ok(fetched.includes("https://shared.team.exe.xyz/models.json"));
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
        name: "managed-sub",
        baseURL: "https://managed-sub.int.exe.xyz",
        catalog: { schema_version: 1, models: [openAIGPTModel("managed")] },
      },
      {
        name: "chatgpt-sub",
        baseURL: "https://chatgpt-sub.int.exe.xyz",
        catalog: { schema_version: 1, models: [openAIGPTModel("chatgpt")] },
      },
    ],
    pricingCatalog,
    (message) => assert.fail(`unexpected warning: ${message}`),
  );

  const openai = infos.get("openai");
  assert.ok(openai);
  assert.equal(openai.config.name, "chatgpt-sub, managed-sub");
  assert.deepEqual(
    openai.config.models?.map((model) => model.id).sort(),
    ["gpt-5.5@chatgpt-sub", "gpt-5.5@managed-sub"],
  );
  assert.equal(openai.modelAliases?.get("gpt-5.5@chatgpt-sub"), "gpt-5.5");
  assert.equal(openai.modelAliases?.get("gpt-5.5@managed-sub"), "gpt-5.5");
  assert.equal(openai.chatGPTModelIds?.has("gpt-5.5@chatgpt-sub"), true);
  assert.equal(openai.chatGPTModelIds?.has("gpt-5.5"), false);
  assert.deepEqual(Array.from(openai.modelIds ?? []).sort(), ["gpt-5.5@chatgpt-sub", "gpt-5.5@managed-sub"]);
});

test("warns once when integration pricing is absent", () => {
  const warnings: string[] = [];
  const infos = providerInfosFromIntegrationCatalogs(
    [
      {
        name: "one",
        baseURL: "https://one.int.exe.xyz",
        catalog: { schema_version: 1, models: [openAIGPTModel("managed")] },
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

  assert.ok(infos.get("openai"));
  assert.equal(warnings.length, 1);
  assert.match(warnings[0] ?? "", /missing pricing for integration model openai\/gpt-5\.5/);
});

test("formats prompt integration availability with at most two names", () => {
  assert.equal(integrationPromptAvailabilityLabel(["beta", "alpha"]), "alpha, beta");
  assert.equal(integrationPromptAvailabilityLabel(["beta", "alpha", "gamma"]), "alpha, beta, ...");
  assert.equal(integrationPromptAvailabilityLabel(["beta", "alpha", "beta", "gamma"]), "alpha, beta, ...");
});
