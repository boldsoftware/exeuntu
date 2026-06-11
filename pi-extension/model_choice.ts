export type ModelChoice = {
  provider: string;
  id: string;
};

export function modelChoiceLabel(model: ModelChoice): string {
  return `${model.provider}/${model.id}`;
}

export function chooseDefaultModel<T extends ModelChoice>(models: T[]): T | undefined {
  return models[0];
}
