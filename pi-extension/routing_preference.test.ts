import assert from "node:assert/strict";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import {
  llmIntegrationPromptDecision,
  parseLLMIntegrationPreference,
  readLLMIntegrationPreference,
  writeLLMIntegrationPreference,
} from "./routing_preference.ts";

test("maps llm integration prompt choices to session and persisted behavior", () => {
  assert.deepEqual(llmIntegrationPromptDecision("Use exe.dev LLM integration [llm]", "Use exe.dev LLM integration [llm]", "I'll configure pi myself"), {
    preference: "use",
    persist: true,
    selectDefaultModel: true,
  });
  assert.deepEqual(llmIntegrationPromptDecision("I'll configure pi myself", "Use exe.dev LLM integration [llm]", "I'll configure pi myself"), {
    preference: "skip",
    persist: true,
    selectDefaultModel: false,
  });
  assert.deepEqual(llmIntegrationPromptDecision(undefined, "Use exe.dev LLM integration [llm]", "I'll configure pi myself"), {
    preference: "skip",
    persist: false,
    selectDefaultModel: false,
  });
});

test("parses only explicit llm integration preferences", () => {
  assert.equal(parseLLMIntegrationPreference({ useExeIntegration: true }), "use");
  assert.equal(parseLLMIntegrationPreference({ useExeIntegration: false }), "skip");
  assert.equal(parseLLMIntegrationPreference({ useExeIntegration: "false" }), undefined);
  assert.equal(parseLLMIntegrationPreference({ useLLMIntegration: false }), undefined);
  assert.equal(parseLLMIntegrationPreference({}), undefined);
  assert.equal(parseLLMIntegrationPreference(null), undefined);
});

test("reads and writes llm integration preference files", () => {
  const dir = mkdtempSync(join(tmpdir(), "pi-routing-pref-"));
  const path = join(dir, "nested", "preference.json");
  try {
    assert.equal(readLLMIntegrationPreference(path), undefined);

    writeLLMIntegrationPreference(path, "skip");
    assert.equal(readLLMIntegrationPreference(path), "skip");

    writeLLMIntegrationPreference(path, "use");
    assert.equal(readLLMIntegrationPreference(path), "use");

    writeFileSync(path, "{");
    assert.equal(readLLMIntegrationPreference(path), undefined);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
