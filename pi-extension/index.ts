import {
  getAgentDir,
  type ExtensionAPI,
  type ExtensionContext,
} from "@mariozechner/pi-coding-agent";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import {
  discoverIntegrationCatalogs,
  fetchJSONWithTimeout,
  integrationPromptAvailabilityLabel,
  integrationProviderDisplayName,
  providerInfosFromIntegrationCatalogs,
  validCatalog,
  type Catalog,
  type IntegrationProviderInfo,
} from "./integration_catalog.ts";
import { chooseDefaultModel, modelChoiceLabel } from "./model_choice.ts";
import {
  exeDevProviderIDsToUnregister,
  type RegisteredProviderConfigSnapshot,
} from "./provider_ownership.ts";
import { rewriteIntegrationProviderPayload } from "./request_rewrite.ts";
import {
  llmIntegrationPromptDecision,
  readLLMIntegrationPreference,
  shouldRegisterLLMIntegrations,
  writeLLMIntegrationPreference,
  type LLMIntegrationPreference,
} from "./routing_preference.ts";

// Reflection and integration endpoints are reachable inside every exe.dev VM.
const REFLECTION_INTEGRATIONS_URL = "https://reflection.int.exe.xyz/integrations";
const FETCH_TIMEOUT_MS = 1500;

// The bundled public catalog contributes pricing and compatibility metadata
// only. Reflection-discovered integrations are the sole source of models and
// provider routes.
const HERE = dirname(fileURLToPath(import.meta.url));
const BUNDLED_PRICING_CATALOG = join(HERE, "catalog.json");
const LLM_INTEGRATION_PREFERENCE_FILE = join(getAgentDir(), "exe-dev-llm-integration.json");

function loadBundledPricingCatalog(): Catalog | undefined {
  if (!existsSync(BUNDLED_PRICING_CATALOG)) return undefined;
  try {
    const catalog = JSON.parse(readFileSync(BUNDLED_PRICING_CATALOG, "utf8")) as unknown;
    if (validCatalog(catalog)) return catalog;
    console.warn(`[pi-exe-dev] ignoring ${BUNDLED_PRICING_CATALOG}: schemaVersion or shape mismatch`);
  } catch (err) {
    console.warn(`[pi-exe-dev] ignoring ${BUNDLED_PRICING_CATALOG}: ${(err as Error).message}`);
  }
  return undefined;
}

function isIntegrationBaseUrl(baseUrl: string | undefined, info: IntegrationProviderInfo): boolean {
  return baseUrl != null && info.baseUrls.has(baseUrl);
}

type CurrentModel = NonNullable<ExtensionContext["model"]>;

let procEnvCache: Map<string, string> | null = null;

// envValue mirrors pi-ai's Bun compiled-binary workaround for sandboxed Linux
// environments where process.env can be empty. It is local to the legacy
// exe.dev kill switch.
function envValue(key: string): string | undefined {
  const value = process.env[key];
  if (value !== undefined) return value;
  if (!process.versions?.bun || Object.keys(process.env).length > 0) return undefined;
  if (procEnvCache === null) {
    procEnvCache = new Map();
    try {
      const data = readFileSync("/proc/self/environ", "utf8");
      for (const entry of data.split("\0")) {
        const idx = entry.indexOf("=");
        if (idx > 0) procEnvCache.set(entry.slice(0, idx), entry.slice(idx + 1));
      }
    } catch {
      // /proc/self/environ may not be readable.
    }
  }
  return procEnvCache.get(key);
}

// Backward-compatible kill switch. Setting EXE_DEV_DISABLE_GATEWAY to a truthy value
// ("1", "true", "yes", or "on", case-insensitive) makes the extension skip
// every LLM integration provider registration. The exe.dev system-prompt injection
// still runs so the model knows it's in a VM, but pi falls back to its
// built-in providers and the user's own credentials.
//
// Allowlisting truthy values rather than blocklisting falsy ones avoids the
// systemd-style trap where EXE_DEV_DISABLE_GATEWAY=off would otherwise
// silently disable integrations. Read once when the extension factory runs;
// /reload reruns the factory and picks up changes.
const TRUTHY_KILL_SWITCH = new Set(["1", "true", "yes", "on"]);
function integrationsDisabled(): boolean {
  const v = envValue("EXE_DEV_DISABLE_GATEWAY");
  if (v == null) return false;
  return TRUTHY_KILL_SWITCH.has(v.toLowerCase());
}

export default async function (pi: ExtensionAPI) {
  // Only activate on exe.dev VMs.
  if (!existsSync("/exe.dev")) return;

  const disabled = integrationsDisabled();
  let llmIntegrationPreference: LLMIntegrationPreference | undefined =
    readLLMIntegrationPreference(LLM_INTEGRATION_PREFERENCE_FILE);

  const discovered =
    disabled || llmIntegrationPreference === "skip"
      ? { found: false, integrations: [] }
      : await discoverIntegrationCatalogs(REFLECTION_INTEGRATIONS_URL, (url) =>
          fetchJSONWithTimeout(url, FETCH_TIMEOUT_MS),
        );
  const pricingCatalog = discovered.found ? loadBundledPricingCatalog() : undefined;
  const integrationNames = discovered.integrations.map((integration) => integration.name);
  const routeLabel = integrationProviderDisplayName(integrationNames);
  const availableIntegrationsLabel = integrationPromptAvailabilityLabel(integrationNames);
  const integrationInfos = providerInfosFromIntegrationCatalogs(discovered.integrations, pricingCatalog);
  if (discovered.found && integrationInfos.size === 0 && !disabled) {
    console.warn(`[pi-exe-dev] LLM integration discovered, but no supported models were available`);
  }

  const registerIntegrations = (): void => {
    for (const [id, info] of integrationInfos) {
      try {
        pi.registerProvider(id, info.config);
      } catch (err) {
        console.warn(`[pi-exe-dev] failed to register provider ${id}: ${(err as Error).message}`);
      }
    }
  };

  const unregisterIntegrations = (): void => {
    for (const id of integrationInfos.keys()) pi.unregisterProvider(id);
  };

  // On the first run, make the models available during Pi's initial model
  // resolution. session_start prompts before the user can send a request and
  // immediately unregisters them if the user opts out.
  if (shouldRegisterLLMIntegrations(llmIntegrationPreference, disabled)) registerIntegrations();

  const reconcileProviderRegistrations = (ctx: ExtensionContext): void => {
    // Provider registrations outlive an extension instance across /reload.
    // The exe-dev namespace is always ours, while unnamespaced legacy ids are
    // removed only when their bound config or models point at an exe.dev route.
    const registry = ctx.modelRegistry as typeof ctx.modelRegistry & {
      getRegisteredProviderConfig?: (providerID: string) => RegisteredProviderConfigSnapshot | undefined;
      getRegisteredProviderIds?: () => readonly string[];
    };
    const models = ctx.modelRegistry.getAll();
    const registeredProviderIDs = new Set(registry.getRegisteredProviderIds?.() ?? []);
    for (const id of integrationInfos.keys()) registeredProviderIDs.add(id);
    for (const id of exeDevProviderIDsToUnregister(
      registeredProviderIDs,
      (providerID) => registry.getRegisteredProviderConfig?.(providerID),
      models,
    )) {
      pi.unregisterProvider(id);
    }

    if (shouldRegisterLLMIntegrations(llmIntegrationPreference, disabled)) registerIntegrations();
  };

  const availableIntegrationModels = (ctx: ExtensionContext): CurrentModel[] => {
    return ctx.modelRegistry
      .getAvailable()
      .filter((model) => {
        const info = integrationInfos.get(model.provider);
        return !!info && isIntegrationBaseUrl(model.baseUrl, info);
      });
  };

  const selectDefaultIntegrationModel = async (ctx: ExtensionContext): Promise<void> => {
    const models = availableIntegrationModels(ctx);
    if (models.length === 0) {
      ctx.ui.notify(`No models were found in ${routeLabel}. Configure pi manually, then use /model to select a model.`, "error");
      return;
    }

    const model = chooseDefaultModel(models);
    if (!model) {
      ctx.ui.notify("No models are available for this choice. Configure pi, then use /model to select a model.", "error");
      return;
    }
    const ok = await pi.setModel(model);
    if (!ok) {
      ctx.ui.notify(`Could not select ${modelChoiceLabel(model)}. Use /model to select a model.`, "error");
      return;
    }
    const theme = ctx.ui.theme;
    ctx.ui.notify(
      `${theme.bold(theme.fg("warning", "Selected model:"))} ${theme.fg("accent", modelChoiceLabel(model))}. ${theme.fg("muted", "Use")} ${theme.fg("accent", "/model")} ${theme.fg("muted", "to select a different model later.")}`,
      "info",
    );
  };

  const promptForLLMIntegrationPreference = async (ctx: ExtensionContext): Promise<void> => {
    if (disabled || integrationInfos.size === 0 || llmIntegrationPreference != null) return;
    if (!ctx.hasUI) return;

    const promptTitle = [
      "Use exe.dev LLM integrations?",
      "Automatically configure pi to use models from this VM's attached LLM integrations instead of configuring pi manually.",
    ].join("\n");
    const availableLabel = `[${availableIntegrationsLabel}]`;
    const useLabel = `Use exe.dev LLM integration ${availableLabel}`;
    const directLabel = "I'll configure pi myself";
    const choice = await ctx.ui.select(promptTitle, [useLabel, directLabel]);
    const decision = llmIntegrationPromptDecision(choice, useLabel, directLabel);

    llmIntegrationPreference = decision.preference;
    if (decision.persist) {
      try {
        writeLLMIntegrationPreference(LLM_INTEGRATION_PREFERENCE_FILE, llmIntegrationPreference);
      } catch (err) {
        ctx.ui.notify(`Could not save LLM routing preference: ${(err as Error).message}`, "warning");
      }
    }

    if (decision.preference === "use") {
      registerIntegrations();
      if (decision.selectDefaultModel) await selectDefaultIntegrationModel(ctx);
    } else {
      unregisterIntegrations();
    }
  };

  pi.on("session_start", async (_event, ctx) => {
    reconcileProviderRegistrations(ctx);
    await promptForLLMIntegrationPreference(ctx);
  });

  pi.on("session_shutdown", () => {
    unregisterIntegrations();
  });

  pi.on("before_provider_request", (event, ctx) => {
    const model = ctx.model;
    if (!model) return undefined;
    const info = integrationInfos.get(model.provider);
    if (!info || !isIntegrationBaseUrl(model.baseUrl, info)) return undefined;
    return rewriteIntegrationProviderPayload(event.payload, {
      modelAliases: info.modelAliases,
      chatGPTModelIds: info.chatGPTModelIds,
      selectedModelID: model.id,
    });
  });

  // Inject exe.dev context into the system prompt.
  pi.on("before_agent_start", async (event) => {
    return {
      systemPrompt:
        event.systemPrompt +
        `

You are running inside an exe.dev VM, which provides HTTPS proxy, auth, email, and more. Docs index: https://exe.dev/docs.md

`,
    };
  });
}
