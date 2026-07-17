export type RegisteredProviderConfigSnapshot = {
  apiKey?: string;
  baseUrl?: string;
};

export type ProviderModelSnapshot = {
  provider: string;
  baseUrl?: string;
};

function isLegacyExeDevIntegrationBaseUrl(baseUrl: string | undefined): boolean {
  if (!baseUrl) return false;
  try {
    const url = new URL(baseUrl);
    return url.hostname.endsWith(".int.exe.xyz") || url.hostname.endsWith(".team.exe.xyz");
  } catch {
    return false;
  }
}

export function isLegacyExeDevProvider(
  providerID: string,
  config: RegisteredProviderConfigSnapshot | undefined,
  models: readonly ProviderModelSnapshot[],
): boolean {
  if (config) return config.apiKey === "gateway" || config.apiKey === "integration";
  return models.some((model) => model.provider === providerID && isLegacyExeDevIntegrationBaseUrl(model.baseUrl));
}

export function exeDevProviderIDsToUnregister(
  registeredProviderIDs: Iterable<string>,
  getConfig: (providerID: string) => RegisteredProviderConfigSnapshot | undefined,
  models: readonly ProviderModelSnapshot[],
): string[] {
  const candidates = new Set(registeredProviderIDs);
  for (const model of models) candidates.add(model.provider);

  return Array.from(candidates).filter(
    (providerID) =>
      providerID.startsWith("exe-dev-") || isLegacyExeDevProvider(providerID, getConfig(providerID), models),
  );
}
