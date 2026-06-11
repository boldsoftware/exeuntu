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
    delete out.max_output_tokens;
    delete out.max_tokens;
    delete out.max_completion_tokens;
    out.store = false;
    moveInstructionInputToInstructions(out);
    changed = true;
  }
  return changed ? out : undefined;
}
