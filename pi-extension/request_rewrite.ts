export type IntegrationProviderPayloadRewrite = {
  modelAliases?: ReadonlyMap<string, string>;
  chatGPTModelIds?: ReadonlySet<string>;
  selectedModelID?: string;
};

function payloadModelID(payload: Record<string, unknown>, selectedModelID: string | undefined): string | undefined {
  return typeof payload.model === "string" ? payload.model : selectedModelID;
}

export function nativeModelIDForGeneratedModel(
  modelID: unknown,
  modelAliases: ReadonlyMap<string, string> | undefined,
): string | undefined {
  if (typeof modelID !== "string") return undefined;
  return modelAliases?.get(modelID);
}

function responseContentText(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .map((part) => {
      if (!part || typeof part !== "object") return "";
      const text = (part as { text?: unknown }).text;
      return typeof text === "string" ? text : "";
    })
    .filter((text) => text.length > 0)
    .join("\n");
}

function isInstructionRole(role: unknown): boolean {
  return role === "system" || role === "developer";
}

function moveInstructionInputToInstructions(payload: Record<string, unknown>): void {
  if (!Array.isArray(payload.input)) return;
  const instructions: string[] = [];
  const input: unknown[] = [];
  for (const item of payload.input) {
    if (item && typeof item === "object" && isInstructionRole((item as { role?: unknown }).role)) {
      const text = responseContentText((item as { content?: unknown }).content);
      if (text) instructions.push(text);
      continue;
    }
    input.push(item);
  }
  if (instructions.length === 0) return;
  const existing = typeof payload.instructions === "string" && payload.instructions.length > 0 ? [payload.instructions] : [];
  payload.instructions = [...existing, ...instructions].join("\n\n");
  payload.input = input;
}

function applyChatGPTCodexDefaults(payload: Record<string, unknown>): void {
  payload.store = false;
  moveInstructionInputToInstructions(payload);

  const text = payload.text;
  payload.text =
    text && typeof text === "object" && !Array.isArray(text)
      ? { verbosity: "low", ...(text as Record<string, unknown>) }
      : { verbosity: "low" };

  const include = Array.isArray(payload.include) ? [...payload.include] : [];
  if (!include.includes("reasoning.encrypted_content")) include.push("reasoning.encrypted_content");
  payload.include = include;

  if (payload.tool_choice === undefined) payload.tool_choice = "auto";
  if (payload.parallel_tool_calls === undefined) payload.parallel_tool_calls = true;
}

export function rewriteIntegrationProviderPayload(
  payload: unknown,
  opts: IntegrationProviderPayloadRewrite,
): unknown | undefined {
  if (!payload || typeof payload !== "object" || Array.isArray(payload)) return undefined;
  const out = { ...(payload as Record<string, unknown>) };
  let changed = false;
  const requestModelID = payloadModelID(out, opts.selectedModelID);
  const nativeModelID = nativeModelIDForGeneratedModel(out.model, opts.modelAliases);
  if (nativeModelID) {
    out.model = nativeModelID;
    changed = true;
  }
  if (requestModelID && opts.chatGPTModelIds?.has(requestModelID)) {
    applyChatGPTCodexDefaults(out);
    changed = true;
  }
  return changed ? out : undefined;
}
