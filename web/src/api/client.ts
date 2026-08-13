import type { Account, DashboardStats, ModelStatsResponse } from "@/types";

const BASE = "/api";

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

async function apiFetch<T>(path: string, init?: RequestInit): Promise<T> {
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    ...(init?.headers as Record<string, string>),
  };
  const res = await fetch(`${BASE}${path}`, {
    ...init,
    headers,
    credentials: "same-origin",
  });

  if (res.status === 401) {
    throw new ApiError(res.status, "Unauthorized");
  }

  if (res.status === 204) {
    return undefined as T;
  }

  if (!res.ok) {
    const text = await res.text();
    let message = text;
    try {
      const body = JSON.parse(text) as { error?: string };
      message = body.error || text;
    } catch {
      // Preserve non-JSON error responses.
    }
    throw new ApiError(res.status, message || `HTTP ${res.status}`);
  }

  return res.json() as Promise<T>;
}

export const api = {
  async login(username: string, password: string): Promise<boolean> {
    const res = await fetch(`${BASE}/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username, password }),
    });
    if (!res.ok) return false;
    return res.ok;
  },

  logout(): Promise<void> {
    return apiFetch<void>("/auth/logout", { method: "POST" });
  },

  checkAuth(): Promise<{ authenticated: boolean }> {
    return fetch(`${BASE}/auth/check`, { credentials: "same-origin" }).then(
      (r) => r.json(),
    );
  },

  getStats(): Promise<DashboardStats> {
    return apiFetch<DashboardStats>("/stats");
  },

  getAccounts(): Promise<Account[]> {
    return apiFetch<Account[]>("/accounts");
  },

  reloadAccounts(accountIds?: number[]): Promise<{ status: string }> {
    return apiFetch<{ status: string }>("/accounts/reload", {
      method: "POST",
      body:
        accountIds === undefined
          ? undefined
          : JSON.stringify({ account_ids: accountIds }),
    });
  },

  getReloadProgressStreamURL(): string {
    return `${BASE}/accounts/reload/stream`;
  },

  getEventsURL(): string {
    return `${BASE}/events`;
  },

  getModelStats(days = 30): Promise<ModelStatsResponse> {
    return apiFetch<ModelStatsResponse>(`/stats/models?days=${days}`);
  },

  deleteAccount(id: number): Promise<void> {
    return apiFetch<void>(`/accounts/${id}`, { method: "DELETE" });
  },

  refreshAccount(id: number): Promise<Account> {
    return apiFetch<Account>(`/accounts/${id}/refresh`, { method: "POST" });
  },

  createAccount(input: {
    api_key: string;
    project_id?: string;
    agent_id?: string;
  }): Promise<Account> {
    return apiFetch<Account>("/accounts", {
      method: "POST",
      body: JSON.stringify(input),
    });
  },

  bulkCreateAccounts(input: {
    keys: string;
    project_id?: string;
    agent_id?: string;
  }): Promise<{
    total: number;
    created: number;
    duplicates: number;
    failed: number;
    results: { api_key_masked?: string; status: string; error?: string }[];
  }> {
    return apiFetch("/accounts/bulk", {
      method: "POST",
      body: JSON.stringify(input),
    });
  },

  setAccountEnabled(id: number, enabled: boolean): Promise<Account> {
    return apiFetch<Account>(`/accounts/${id}`, {
      method: "PATCH",
      body: JSON.stringify({ enabled }),
    });
  },
};
