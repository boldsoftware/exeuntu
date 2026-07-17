import assert from "node:assert/strict";
import test from "node:test";
import { exeDevProviderIDsToUnregister, isLegacyExeDevProvider } from "./provider_ownership.ts";

test("recognizes only legacy exe.dev provider registrations", () => {
  assert.equal(
    isLegacyExeDevProvider("openai", { apiKey: "gateway", baseUrl: "https://example.com" }, []),
    true,
  );
  assert.equal(
    isLegacyExeDevProvider("xai", { apiKey: "integration", baseUrl: "https://example.com" }, []),
    true,
  );
  assert.equal(
    isLegacyExeDevProvider("anthropic", { apiKey: "integration", baseUrl: "https://llm.int.exe.xyz" }, []),
    true,
  );
  assert.equal(
    isLegacyExeDevProvider("openai", undefined, [
      { provider: "openai", baseUrl: "https://shared.team.exe.xyz/v1" },
    ]),
    true,
  );
  assert.equal(
    isLegacyExeDevProvider("openai", { apiKey: "user-key", baseUrl: "https://api.openai.com/v1" }, [
      { provider: "openai", baseUrl: "https://api.openai.com/v1" },
    ]),
    false,
  );
  assert.equal(
    isLegacyExeDevProvider("direct-internal", { apiKey: "user-key", baseUrl: "https://other.int.exe.xyz" }, [
      { provider: "direct-internal", baseUrl: "https://other.int.exe.xyz" },
    ]),
    false,
  );
  assert.equal(
    isLegacyExeDevProvider("openai", undefined, [
      { provider: "anthropic", baseUrl: "https://llm.int.exe.xyz" },
    ]),
    false,
  );
});

test("discovers extension-owned provider ids without a vendor list", () => {
  const configs = new Map([
    ["legacy-custom", { apiKey: "integration", baseUrl: "https://example.com" }],
    ["direct-custom", { apiKey: "user-key", baseUrl: "https://provider.example/v1" }],
  ]);
  const models = [
    { provider: "legacy-model-only", baseUrl: "https://llm.int.exe.xyz/legacy-model-only/v1" },
    { provider: "direct-custom", baseUrl: "https://provider.example/v1" },
  ];

  assert.deepEqual(
    exeDevProviderIDsToUnregister(
      ["exe-dev-openrouter", "legacy-custom", "direct-custom"],
      (providerID) => configs.get(providerID),
      models,
    ).sort(),
    ["exe-dev-openrouter", "legacy-custom", "legacy-model-only"],
  );
});
