import assert from "node:assert/strict";
import test from "node:test";
import { chooseDefaultModel, modelChoiceLabel } from "./model_choice.ts";

test("labels models by provider and id", () => {
  assert.equal(modelChoiceLabel({ provider: "openai", id: "gpt-5.4" }), "openai/gpt-5.4");
});

test("chooses the first available model from pi's runtime order", () => {
  const models = [
    { provider: "openai", id: "gpt-5.5" },
    { provider: "anthropic", id: "claude-opus-4-8" },
  ];

  assert.deepEqual(chooseDefaultModel(models), { provider: "openai", id: "gpt-5.5" });
  assert.equal(chooseDefaultModel([]), undefined);
});
