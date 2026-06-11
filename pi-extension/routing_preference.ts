import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname } from "node:path";

export type LLMIntegrationPreference = "use" | "skip";

export type LLMIntegrationPromptDecision = {
  preference: LLMIntegrationPreference;
  persist: boolean;
  selectDefaultModel: boolean;
};

export function llmIntegrationPromptDecision(
  choice: unknown,
  useLabel: string,
  directLabel: string,
): LLMIntegrationPromptDecision {
  if (choice === useLabel) return { preference: "use", persist: true, selectDefaultModel: true };
  if (choice === directLabel) return { preference: "skip", persist: true, selectDefaultModel: false };
  return { preference: "skip", persist: false, selectDefaultModel: false };
}

export function parseLLMIntegrationPreference(value: unknown): LLMIntegrationPreference | undefined {
  if (!value || typeof value !== "object" || Array.isArray(value)) return undefined;
  const useExeIntegration = (value as { useExeIntegration?: unknown }).useExeIntegration;
  if (useExeIntegration === true) return "use";
  if (useExeIntegration === false) return "skip";
  return undefined;
}

export function readLLMIntegrationPreference(path: string): LLMIntegrationPreference | undefined {
  if (!existsSync(path)) return undefined;
  try {
    return parseLLMIntegrationPreference(JSON.parse(readFileSync(path, "utf8")) as unknown);
  } catch {
    return undefined;
  }
}

export function writeLLMIntegrationPreference(path: string, preference: LLMIntegrationPreference): void {
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(
    path,
    `${JSON.stringify({ version: 1, useExeIntegration: preference === "use" }, null, 2)}\n`,
  );
}
