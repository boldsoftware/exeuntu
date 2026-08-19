import assert from "node:assert/strict";
import test from "node:test";

import exeDevPlugin from "./exe-dev.js";

const reflectionURL = "https://reflection.int.exe.xyz/integrations";

function response(body, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

function catalog(models) {
  return { schema_version: 1, models };
}

async function configureWith(fetchImpl, config = {}) {
  const oldFetch = globalThis.fetch;
  const oldTestMode = process.env.EXE_DEV_PLUGIN_TEST;
  const oldDisable = process.env.EXE_DEV_DISABLE_GATEWAY;
  globalThis.fetch = fetchImpl;
  process.env.EXE_DEV_PLUGIN_TEST = "1";
  delete process.env.EXE_DEV_DISABLE_GATEWAY;
  try {
    const hooks = await exeDevPlugin({});
    assert.equal(typeof hooks.config, "function");
    await hooks.config(config);
    return config;
  } finally {
    globalThis.fetch = oldFetch;
    if (oldTestMode === undefined) delete process.env.EXE_DEV_PLUGIN_TEST;
    else process.env.EXE_DEV_PLUGIN_TEST = oldTestMode;
    if (oldDisable === undefined) delete process.env.EXE_DEV_DISABLE_GATEWAY;
    else process.env.EXE_DEV_DISABLE_GATEWAY = oldDisable;
  }
}

test("discovers LLM integrations and injects protocol-specific providers", async () => {
  const requested = [];
  const config = await configureWith(async (url) => {
    requested.push(String(url));
    switch (String(url)) {
      case reflectionURL:
        return response({
          integrations: [
            { name: "llm", type: "llm" },
            { name: "shared", type: "llm", team: true },
            { name: "github", type: "github" },
          ],
        });
      case "https://llm.int.exe.xyz/models.json":
        return response(catalog([
          {
            id: "openai/gpt-test",
            name: "GPT Test",
            provider: "openai",
            native_id: "gpt-test-native",
            apis: ["openai_chat", "openai_responses"],
            architecture: { input_modalities: ["text", "image"], output_modalities: ["text"] },
            limits: { context_window: 200000, max_output_tokens: 32000 },
            supported_parameters: ["messages", "tools"],
          },
          {
            id: "anthropic/claude-test",
            name: "Claude Test",
            provider: "anthropic",
            native_id: "claude-test-native",
            apis: ["openai_responses", "anthropic_messages"],
            limits: { context_window: 100000 },
          },
          {
            id: "fireworks/glm-test",
            name: "GLM Test",
            provider: "fireworks",
            native_id: "accounts/fireworks/models/glm-test",
            apis: ["openai_chat"],
          },
          {
            id: "openai/embedding-test",
            provider: "openai",
            native_id: "embedding-test",
            apis: ["openai_embeddings"],
          },
        ]));
      case "https://shared.team.exe.xyz/models.json":
        return response(catalog([
          {
            id: "xai/grok-test",
            name: "Grok Test",
            provider: "xai",
            native_id: "grok-test-native",
            apis: ["openai_chat", "openai_responses"],
          },
        ]));
      default:
        throw new Error(`unexpected URL ${url}`);
    }
  });

  assert.deepEqual(new Set(requested), new Set([
    reflectionURL,
    "https://llm.int.exe.xyz/models.json",
    "https://shared.team.exe.xyz/models.json",
  ]));
  assert.equal(config.model, undefined, "plugin must not silently change the user's default model");

  const openai = config.provider["exe-p-3-llm-3-llm-6-openai-responses"];
  assert.equal(openai.npm, "@ai-sdk/openai");
  assert.equal(openai.options.baseURL, "https://llm.int.exe.xyz/openai/v1");
  assert.equal(openai.options.apiKey, "implicit");
  assert.deepEqual(openai.models["gpt-test"], {
    id: "gpt-test-native",
    name: "GPT Test",
    attachment: true,
    tool_call: true,
    limit: { context: 200000, output: 32000 },
    modalities: { input: ["text", "image"], output: ["text"] },
  });

  const anthropic = config.provider["exe-p-3-llm-3-llm-9-anthropic-messages"];
  assert.equal(anthropic.npm, "@ai-sdk/anthropic");
  assert.equal(anthropic.options.baseURL, "https://llm.int.exe.xyz/anthropic/v1");
  assert.equal(anthropic.models["claude-test"].id, "claude-test-native");
  assert.deepEqual(anthropic.models["claude-test"].limit, { context: 100000, output: 4096 });

  const chat = config.provider["exe-p-3-llm-3-llm-9-fireworks-chat"];
  assert.equal(chat.npm, "@ai-sdk/openai-compatible");
  assert.equal(chat.options.baseURL, "https://llm.int.exe.xyz/fireworks/v1");
  assert.equal(chat.models["glm-test"].id, "accounts/fireworks/models/glm-test");
  assert.equal(chat.models["embedding-test"], undefined);

  const teamResponses = config.provider["exe-t-6-shared-6-shared-3-xai-responses"];
  assert.equal(teamResponses.npm, "@ai-sdk/openai");
  assert.equal(teamResponses.options.baseURL, "https://shared.team.exe.xyz/xai/v1");
  assert.equal(teamResponses.models["grok-test"].id, "grok-test-native");
});

test("uses integration help URLs and preserves user-owned provider config", async () => {
  const userProvider = { name: "mine", models: { custom: { name: "Custom" } } };
  const input = {
    provider: { "exe-p-3-llm-10-custom-llm-6-openai-responses": userProvider },
  };
  const originalProviderMap = input.provider;
  const config = await configureWith(async (url) => {
    switch (String(url)) {
      case reflectionURL:
        return response({
          integrations: [{
            name: "llm",
            type: "llm",
            help: "Use https://custom-llm.int.exe.xyz/v1/models for this VM",
          }],
        });
      case "https://custom-llm.int.exe.xyz/models.json":
        return response(catalog([{
          id: "openai/gpt-test",
          provider: "openai",
          native_id: "gpt-test",
          apis: ["openai_responses"],
        }]));
      default:
        throw new Error(`unexpected URL ${url}`);
    }
  }, input);

  assert.equal(config.provider["exe-p-3-llm-10-custom-llm-6-openai-responses"], userProvider);
  assert.notEqual(config.provider, originalProviderMap);
  assert.deepEqual(originalProviderMap, { "exe-p-3-llm-10-custom-llm-6-openai-responses": userProvider });
});

test("provider IDs are stable when sanitized integration identities collide", async () => {
  const configureOrder = async (integrations) => configureWith(async (url) => {
    switch (String(url)) {
      case reflectionURL:
        return response({ integrations });
      case "https://one.int.exe.xyz/models.json":
      case "https://two.int.exe.xyz/models.json":
        return response(catalog([{
          id: "openai/gpt-test",
          provider: "openai",
          native_id: "gpt-test",
          apis: ["openai_responses"],
        }]));
      case "https://dup.int.exe.xyz/models.json":
        return response({}, 404);
      default:
        throw new Error(`unexpected URL ${url}`);
    }
  });
  const one = { name: "dup", type: "llm", help: "https://one.int.exe.xyz/v1" };
  const two = { name: "dup", type: "llm", help: "https://two.int.exe.xyz/v1" };
  const first = await configureOrder([one, two]);
  const second = await configureOrder([two, one]);

  const providerIDsByBaseURL = (config) => Object.fromEntries(
    Object.entries(config.provider).map(([id, provider]) => [provider.options.baseURL, id]),
  );
  assert.deepEqual(providerIDsByBaseURL(first), providerIDsByBaseURL(second));
  assert.equal(Object.keys(first.provider).length, 2);
});

test("personal and team integration scopes cannot collide", async () => {
  const config = await configureWith(async (url) => {
    switch (String(url)) {
      case reflectionURL:
        return response({ integrations: [
          { name: "team-foo", type: "llm" },
          { name: "foo", type: "llm", team: true },
        ] });
      case "https://team-foo.int.exe.xyz/models.json":
      case "https://foo.team.exe.xyz/models.json":
        return response(catalog([{
          id: "openai/gpt-test",
          provider: "openai",
          native_id: "gpt-test",
          apis: ["openai_responses"],
        }]));
      default:
        throw new Error(`unexpected URL ${url}`);
    }
  });

  assert.ok(config.provider["exe-p-8-team-foo-8-team-foo-6-openai-responses"]);
  assert.ok(config.provider["exe-t-3-foo-3-foo-6-openai-responses"]);
  assert.equal(Object.keys(config.provider).length, 2);
});

test("provider ID component boundaries cannot collide", async () => {
  const config = await configureWith(async (url) => {
    switch (String(url)) {
      case reflectionURL:
        return response({ integrations: [
          { name: "foo-bar", type: "llm" },
          { name: "foo", type: "llm" },
        ] });
      case "https://foo-bar.int.exe.xyz/models.json":
        return response(catalog([{
          id: "openai/gpt-test",
          provider: "openai",
          native_id: "gpt-test",
          apis: ["openai_responses"],
        }]));
      case "https://foo.int.exe.xyz/models.json":
        return response(catalog([{
          id: "bar-openai/gpt-test",
          provider: "bar-openai",
          native_id: "gpt-test",
          apis: ["openai_responses"],
        }]));
      default:
        throw new Error(`unexpected URL ${url}`);
    }
  });

  assert.ok(config.provider["exe-p-7-foo-bar-7-foo-bar-6-openai-responses"]);
  assert.ok(config.provider["exe-p-3-foo-3-foo-10-bar-openai-responses"]);
  assert.equal(Object.keys(config.provider).length, 2);
});

test("omits ChatGPT-account models that need payload rewriting", async () => {
  const warnings = [];
  const oldWarn = console.warn;
  console.warn = (message) => warnings.push(String(message));
  try {
    const config = await configureWith(async (url) => {
      switch (String(url)) {
        case reflectionURL:
          return response({ integrations: [{ name: "llm", type: "llm" }] });
        case "https://llm.int.exe.xyz/models.json":
          return response(catalog([{
            id: "openai/chatgpt-test",
            provider: "openai",
            native_id: "chatgpt-test",
            apis: ["openai_responses"],
            exe_dev: { mode: "chatgpt" },
          }]));
        default:
          throw new Error(`unexpected URL ${url}`);
      }
    });
    assert.deepEqual(config.provider, {});
    assert.equal(warnings.length, 1);
    assert.match(warnings[0], /ChatGPT-account models are not yet supported/);
  } finally {
    console.warn = oldWarn;
  }
});

test("fails open when reflection or catalogs are unavailable", async () => {
  const warnings = [];
  const oldWarn = console.warn;
  console.warn = (message) => warnings.push(String(message));
  try {
    const config = await configureWith(async () => {
      throw new Error("offline");
    }, { provider: { mine: { name: "Mine" } } });
    assert.deepEqual(config, { provider: { mine: { name: "Mine" } } });
    assert.equal(warnings.length, 1);
    assert.match(warnings[0], /reflection fetch failed/);
  } finally {
    console.warn = oldWarn;
  }
});

test("kill switch disables discovery", async () => {
  const oldFetch = globalThis.fetch;
  const oldTestMode = process.env.EXE_DEV_PLUGIN_TEST;
  const oldDisable = process.env.EXE_DEV_DISABLE_GATEWAY;
  globalThis.fetch = async () => {
    throw new Error("fetch should not run");
  };
  process.env.EXE_DEV_PLUGIN_TEST = "1";
  process.env.EXE_DEV_DISABLE_GATEWAY = "true";
  try {
    const hooks = await exeDevPlugin({});
    const config = {};
    await hooks.config(config);
    assert.deepEqual(config, {});
  } finally {
    globalThis.fetch = oldFetch;
    if (oldTestMode === undefined) delete process.env.EXE_DEV_PLUGIN_TEST;
    else process.env.EXE_DEV_PLUGIN_TEST = oldTestMode;
    if (oldDisable === undefined) delete process.env.EXE_DEV_DISABLE_GATEWAY;
    else process.env.EXE_DEV_DISABLE_GATEWAY = oldDisable;
  }
});

test("does nothing outside exeuntu", async () => {
  const oldTestMode = process.env.EXE_DEV_PLUGIN_TEST;
  process.env.EXE_DEV_PLUGIN_TEST = "0";
  try {
    const hooks = await exeDevPlugin({});
    const config = {};
    await hooks.config(config);
    assert.deepEqual(config, {});
  } finally {
    if (oldTestMode === undefined) delete process.env.EXE_DEV_PLUGIN_TEST;
    else process.env.EXE_DEV_PLUGIN_TEST = oldTestMode;
  }
});
