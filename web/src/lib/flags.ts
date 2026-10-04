export type Value = { type: "boolean" | "json"; data: unknown };
export type Rule = {
  attribute: string;
  operator: string;
  values: unknown[];
  value: Value;
};
export type Rollout = { traffic_bp: number; salt: string; value: Value };
export type Flag = {
  project_id: string;
  environment_id: string;
  flag_id: string;
  key: string;
  type: Value["type"];
  revision: number;
  default: Value;
  safe: Value;
  rules: Rule[];
  rollout?: Rollout;
  killed: boolean;
  experiment?: { run_id: string; traffic_bp: number };
};
export type Decision = {
  value: Value;
  reason: string;
  revision: number;
  run_id?: string;
  variant_id?: string;
  decision_id: string;
};
