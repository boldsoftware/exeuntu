import assert from "node:assert/strict";
import test from "node:test";

import plugin from "./exe-dev.js";

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

// setupWith runs the plugin against a fake OpenCode 2 context with the given
// fetch and environment, and returns the provider records it registered,
// keyed by provider ID.
async function setupWith(fetchImpl, env = {}) {
  env = { EXE_DEV_PLUGIN_TEST: "1", EXE_DEV_DISABLE_GATEWAY: undefined, ...env };
  const oldFetch = globalThis.fetch;
  const oldEnv = Object.fromEntries(Object.keys(env).map((key) => [key, process.env[key]]));
  const setEnv = (values) => {
    for (const [key, value] of Object.entries(values)) {
      if (value === undefined) delete process.env[key];
      else process.env[key] = value;
    }
  };
  globalThis.fetch = fetchImpl;
  setEnv(env);
  const providers = {};
  const editor = {
    add: (record) => {
      assert.equal(providers[record.info.id], undefined, `duplicate provider ${record.info.id}`);
      providers[record.info.id] = record;
    },
  };
  try {
    await plugin.setup({ provider: { transform: async (callback) => callback(editor) } });
  } finally {
    globalThis.fetch = oldFetch;
    setEnv(oldEnv);
  }
  return providers;
}

// captureWarnings runs fn and returns its result with the console.warn
// messages it logged.
async function captureWarnings(fn) {
  const warnings = [];
  const oldWarn = console.warn;
  console.warn = (message) => warnings.push(String(message));
  try {
    return { result: await fn(), warnings };
  } finally {
    console.warn = oldWarn;
  }
}

function modelsByID(record) {
  return Object.fromEntries(record.models.map((model) => [model.id, model]));
}

test("discovers LLM integrations and injects protocol-specific providers", async () => {
  const requested = [];
  const providers = await setupWith(async (url) => {
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
  const openai = providers["exe-p-3-llm-3-llm-6-openai-responses"];
  assert.deepEqual(openai.info, {
    id: "exe-p-3-llm-3-llm-6-openai-responses",
    name: "exe.dev llm · openai · OpenAI Responses",
    activation: "enabled",
    package: "@opencode/ai/providers/openai/responses",
    settings: { baseURL: "https://llm.int.exe.xyz/openai/v1", apiKey: "implicit" },
  });
  assert.deepEqual(modelsByID(openai)["gpt-test"], {
    id: "gpt-test",
    modelID: "gpt-test-native",
    providerID: "exe-p-3-llm-3-llm-6-openai-responses",
    name: "GPT Test",
    capabilities: { tools: true, input: ["text", "image"], output: ["text"] },
    variants: [],
    time: { released: 0 },
    cost: [],
    status: "active",
    enabled: true,
    limit: { context: 200000, output: 32000 },
  });

  const anthropic = providers["exe-p-3-llm-3-llm-9-anthropic-messages"];
  assert.equal(anthropic.info.package, "@opencode/ai/providers/anthropic");
  assert.equal(anthropic.info.settings.baseURL, "https://llm.int.exe.xyz/anthropic/v1");
  const claude = modelsByID(anthropic)["claude-test"];
  assert.equal(claude.modelID, "claude-test-native");
  assert.equal(claude.capabilities.tools, false);
  assert.deepEqual(claude.capabilities.input, ["text"]);
  assert.deepEqual(claude.limit, { context: 100000, output: 4096 });

  const chat = providers["exe-p-3-llm-3-llm-9-fireworks-chat"];
  assert.equal(chat.info.package, "@opencode/ai/providers/openai-compatible");
  assert.equal(chat.info.settings.baseURL, "https://llm.int.exe.xyz/fireworks/v1");
  assert.equal(modelsByID(chat)["glm-test"].modelID, "accounts/fireworks/models/glm-test");
  assert.equal(modelsByID(chat)["embedding-test"], undefined);

  const teamResponses = providers["exe-t-6-shared-6-shared-3-xai-responses"];
  assert.equal(teamResponses.info.package, "@opencode/ai/providers/openai/responses");
  assert.equal(teamResponses.info.settings.baseURL, "https://shared.team.exe.xyz/xai/v1");
  assert.equal(modelsByID(teamResponses)["grok-test"].modelID, "grok-test-native");
  assert.equal(Object.keys(providers).length, 4);
});

test("uses integration help URLs", async () => {
  const providers = await setupWith(async (url) => {
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
  });

  assert.deepEqual(Object.keys(providers), ["exe-p-3-llm-10-custom-llm-6-openai-responses"]);
  assert.equal(
    providers["exe-p-3-llm-10-custom-llm-6-openai-responses"].info.settings.baseURL,
    "https://custom-llm.int.exe.xyz/openai/v1",
  );
});

test("provider IDs are stable when sanitized integration identities collide", async () => {
  const setupOrder = async (integrations) => setupWith(async (url) => {
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
  const first = await setupOrder([one, two]);
  const second = await setupOrder([two, one]);

  const providerIDsByBaseURL = (providers) => Object.fromEntries(
    Object.entries(providers).map(([id, provider]) => [provider.info.settings.baseURL, id]),
  );
  assert.deepEqual(providerIDsByBaseURL(first), providerIDsByBaseURL(second));
  assert.equal(Object.keys(first).length, 2);
});

test("personal and team integration scopes cannot collide", async () => {
  const providers = await setupWith(async (url) => {
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

  assert.ok(providers["exe-p-8-team-foo-8-team-foo-6-openai-responses"]);
  assert.ok(providers["exe-t-3-foo-3-foo-6-openai-responses"]);
  assert.equal(Object.keys(providers).length, 2);
});

test("provider ID component boundaries cannot collide", async () => {
  const providers = await setupWith(async (url) => {
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

  assert.ok(providers["exe-p-7-foo-bar-7-foo-bar-6-openai-responses"]);
  assert.ok(providers["exe-p-3-foo-3-foo-10-bar-openai-responses"]);
  assert.equal(Object.keys(providers).length, 2);
});

test("omits ChatGPT-account models that need payload rewriting", async () => {
  const { result: providers, warnings } = await captureWarnings(() => setupWith(async (url) => {
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
  }));
  assert.deepEqual(providers, {});
  assert.equal(warnings.length, 1);
  assert.match(warnings[0], /ChatGPT-account models are not yet supported/);
});

test("retries once when the first fetch attempt fails", async () => {
  const requested = [];
  const providers = await setupWith(async (url) => {
    requested.push(String(url));
    const attempt = requested.filter((seen) => seen === String(url)).length;
    if (attempt === 1) throw new Error("The operation timed out.");
    switch (String(url)) {
      case reflectionURL:
        return response({ integrations: [{ name: "llm", type: "llm" }] });
      case "https://llm.int.exe.xyz/models.json":
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

  assert.deepEqual(requested, [
    reflectionURL,
    reflectionURL,
    "https://llm.int.exe.xyz/models.json",
    "https://llm.int.exe.xyz/models.json",
  ]);
  assert.ok(providers["exe-p-3-llm-3-llm-6-openai-responses"]);
});

// An HTTP response means the endpoint answered: probing base URL candidates
// 404s by design, and retrying those would double discovery latency on the
// path the retry exists to keep fast.
// An HTTP response means the endpoint answered: probing base URL candidates
// 404s by design, and retrying those would double discovery latency on the
// path the retry exists to keep fast.
test("does not retry a fetch the server answered with an error status", async () => {
  const requested = [];
  const { result: providers, warnings } = await captureWarnings(() => setupWith(async (url) => {
    requested.push(String(url));
    return response({ error: "unavailable" }, 503);
  }));
  assert.deepEqual(requested, [reflectionURL]);
  assert.deepEqual(providers, {});
  assert.equal(warnings.length, 1);
  assert.match(warnings[0], /reflection fetch failed/);
});

test("fails open when reflection or catalogs are unavailable", async () => {
  const { result: providers, warnings } = await captureWarnings(() => setupWith(async () => {
    throw new Error("offline");
  }));
  assert.deepEqual(providers, {});
  assert.equal(warnings.length, 1);
  assert.match(warnings[0], /reflection fetch failed/);
});

const noFetch = async () => {
  throw new Error("fetch should not run");
};

test("kill switch disables discovery", async () => {
  assert.deepEqual(await setupWith(noFetch, { EXE_DEV_DISABLE_GATEWAY: "true" }), {});
});

test("does nothing outside exeuntu", async () => {
  assert.deepEqual(await setupWith(noFetch, { EXE_DEV_PLUGIN_TEST: "0" }), {});
});
