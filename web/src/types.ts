export interface Account {
  id: number;
  email: string;
  name: string;
  api_key_masked: string;
  org_id?: string;
  balance: number;
  balance_unlimited: boolean;
  balance_at?: string;
  status:
    "active" | "exhausted" | "disabled" | "invalid" | "initializing" | "error";
  enabled: boolean;
  project_id?: string;
  agent_id?: string;
  last_error?: string;
  disabled_until?: string;
}

export interface DashboardStats {
  account_count: number;
  active_count: number;
  exhausted_count: number;
  invalid_count: number;
  error_count: number;
  initializing_count: number;
  disabled_count: number;
  total_balance: number;
  unlimited_count: number;
  total_calls: number;
  daily_calls: DailyPoint[];
}

export interface DailyPoint {
  date: string;
  count: number;
}

export interface ReloadProgressResponse {
  running: boolean;
  total: number;
  done: number;
  exhausted: number;
  invalid: number;
}

export interface ModelStatPoint {
  date: string;
  calls: number;
  input_tokens: number;
  output_tokens: number;
  credits: number;
}

export interface ModelStatsResponse {
  models: string[];
  daily: Record<string, ModelStatPoint[]>;
}
