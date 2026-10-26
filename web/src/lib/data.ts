import { useQuery } from "@tanstack/react-query";
import type {
  Account,
  CompetitionSummary,
  DashboardData,
  DatasetMeta,
  JsonRecord,
  MoneyHistory,
  ShipDetail,
  StandingsHistory,
} from "./types";
import { customMoneyRange, type MoneyPreferences } from "./money-history";

async function fetchJSON<T>(path: string, signal: AbortSignal): Promise<T> {
  const response = await fetch(path, {
    signal,
    cache: "no-store",
    headers: { Accept: "application/json" },
  });

  if (!response.ok) {
    throw new Error(`The recording service returned HTTP ${response.status}.`);
  }

  return response.json() as Promise<T>;
}

export function useDashboard() {
  return useQuery({
    queryKey: ["dashboard"],
    queryFn: ({ signal }) => fetchJSON<DashboardData>("/api/dashboard", signal),
    refetchInterval: 5_000,
    refetchIntervalInBackground: false,
    refetchOnWindowFocus: true,
    refetchOnReconnect: true,
    staleTime: 4_000,
    retry: 1,
  });
}

export function useHistory<T>(accountID: string | undefined, kind: string) {
  return useQuery({
    queryKey: ["history", accountID, kind],
    queryFn: ({ signal }) => {
      const query = new URLSearchParams({
        account_id: accountID ?? "",
        kind,
        limit: "500",
      });
      return fetchJSON<T[]>(`/api/history?${query}`, signal);
    },
    enabled: Boolean(accountID),
    refetchInterval: 30_000,
    refetchIntervalInBackground: false,
    staleTime: 20_000,
    retry: 1,
  });
}

export function useMoneyHistory(
  accountID: string,
  preferences: MoneyPreferences,
) {
  const custom =
    preferences.range === "custom"
      ? customMoneyRange(preferences.from, preferences.to)
      : undefined;
  return useQuery({
    queryKey: [
      "money-history",
      accountID,
      preferences.range,
      custom?.from,
      custom?.to,
    ],
    queryFn: ({ signal }) => {
      const query = new URLSearchParams({
        account_id: accountID,
        range: preferences.range,
        ...(custom ?? {}),
      });
      return fetchJSON<MoneyHistory>(`/api/money-history?${query}`, signal);
    },
    enabled:
      Boolean(accountID) && (preferences.range !== "custom" || Boolean(custom)),
    refetchInterval: () =>
      custom && Date.parse(custom.to) < Date.now() ? false : 30_000,
    refetchIntervalInBackground: false,
    refetchOnWindowFocus: true,
    staleTime: 20_000,
    retry: 1,
  });
}

export function useStandingsHistory(
  accountID: string,
  range: Exclude<MoneyPreferences["range"], "custom">,
  top: number,
) {
  return useQuery({
    queryKey: ["standings-history", accountID, range, top],
    queryFn: ({ signal }) => {
      const query = new URLSearchParams({
        account_id: accountID,
        range,
        top: String(top),
      });
      return fetchJSON<StandingsHistory>(
        `/api/standings-history?${query}`,
        signal,
      );
    },
    enabled: Boolean(accountID),
    refetchInterval: 30_000,
    refetchIntervalInBackground: false,
    refetchOnWindowFocus: true,
    staleTime: 20_000,
    retry: 1,
  });
}

export function useCompetition(accountID: string) {
  return useQuery({
    queryKey: ["competition", accountID],
    queryFn: ({ signal }) => {
      const query = new URLSearchParams({ account_id: accountID });
      return fetchJSON<CompetitionSummary>(`/api/competition?${query}`, signal);
    },
    enabled: Boolean(accountID),
    refetchInterval: 30_000,
    refetchIntervalInBackground: false,
    refetchOnWindowFocus: true,
    staleTime: 20_000,
    retry: 1,
  });
}

export function dataset<T>(
  account: Account | undefined,
  key: string,
): T | undefined {
  const separator = key.indexOf("/");
  const value =
    separator === -1
      ? account?.snapshot?.[key]
      : record(account?.snapshot?.[key.slice(0, separator)])[
          key.slice(separator + 1)
        ];
  return value === null || value === undefined ? undefined : (value as T);
}

export function metadata(
  account: Account | undefined,
  key: string,
): DatasetMeta {
  const all = dataset<Record<string, DatasetMeta>>(account, "_meta");
  return all?.[key] ?? {};
}

export function record(value: unknown): JsonRecord {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? (value as JsonRecord)
    : {};
}

export function rows(value: unknown): JsonRecord[] {
  if (!Array.isArray(value)) {
    return [];
  }
  return value.filter(
    (item): item is JsonRecord =>
      item !== null && typeof item === "object" && !Array.isArray(item),
  );
}

export function text(value: unknown, fallback = "Not recorded"): string {
  return typeof value === "string" && value.length > 0 ? value : fallback;
}

export function number(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value)
    ? value
    : undefined;
}

export function fleet(account: Account | undefined): ShipDetail[] {
  const ships = dataset<ShipDetail[]>(account, "ships") ?? [];
  const details =
    dataset<Record<string, ShipDetail>>(account, "ship_details") ?? {};
  return ships.map((item) =>
    shipSource(account, item.ship.id) === "ships"
      ? item
      : (details[item.ship.id] ?? item),
  );
}

export function shipSource(
  account: Account | undefined,
  shipID: string,
): string {
  const childKey = `ship_details/${shipID}`;
  const childUpdated = Date.parse(metadata(account, childKey).updated_at ?? "");
  const listUpdated = Date.parse(metadata(account, "ships").updated_at ?? "");
  return Number.isFinite(childUpdated) &&
    (!Number.isFinite(listUpdated) || childUpdated > listUpdated)
    ? childKey
    : "ships";
}
