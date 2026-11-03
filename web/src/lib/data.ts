import { keepPreviousData, useQuery } from "@tanstack/react-query";
import type {
  Account,
  BulletinBoard,
  BulletinThreadView,
  CompetitionSummary,
  DashboardData,
  DatasetMeta,
  JsonRecord,
  LoanReport,
  MoneyHistory,
  RivalReport,
  ShipDetail,
  StandingsHistory,
  WarningReport,
} from "./types";
import { customMoneyRange, type MoneyPreferences } from "./money-history";

async function fetchJSON<T>(
  path: string,
  signal: AbortSignal,
  tooLargeMessage?: string,
) {
  const response = await fetch(path, {
    signal,
    cache: "no-store",
    headers: { Accept: "application/json" },
  });

  if (!response.ok) {
    if (tooLargeMessage) {
      const failure: unknown = await response.json().catch(() => null);
      if (
        failure !== null &&
        typeof failure === "object" &&
        "error" in failure &&
        failure.error === "response_too_large"
      ) {
        throw new Error(tooLargeMessage);
      }
    }
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

export function useWarnings() {
  return useQuery({
    queryKey: ["warnings"],
    queryFn: ({ signal }) => fetchJSON<WarningReport>("/api/warnings", signal),
    refetchInterval: 30_000,
    refetchIntervalInBackground: false,
    refetchOnWindowFocus: true,
    refetchOnReconnect: true,
    staleTime: 20_000,
    retry: 1,
  });
}

export function useLoans(accountID: string) {
  return useQuery({
    queryKey: ["loans", accountID],
    queryFn: ({ signal }) => {
      const query = new URLSearchParams();
      if (accountID) {
        query.set("account_id", accountID);
      }
      return fetchJSON<LoanReport>(
        `/api/loans${query.size ? `?${query}` : ""}`,
        signal,
        "Loan history too large to display.",
      );
    },
    refetchInterval: 30_000,
    refetchIntervalInBackground: false,
    refetchOnWindowFocus: true,
    refetchOnReconnect: true,
    staleTime: 20_000,
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
      preferences.period,
      custom?.from,
      custom?.to,
    ],
    queryFn: ({ signal }) => {
      const query = new URLSearchParams({
        account_id: accountID,
        range: preferences.range,
        period: preferences.period,
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

export function useRivals(accountID: string) {
  return useQuery({
    queryKey: ["rivals", accountID],
    queryFn: ({ signal }) => {
      const query = new URLSearchParams({ account_id: accountID });
      return fetchJSON<RivalReport>(`/api/rivals?${query}`, signal);
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

export function useBulletinBoard(query: {
  q: string;
  company: string;
  limit: number;
}) {
  return useQuery({
    queryKey: ["bulletin", query],
    queryFn: ({ signal }) => {
      const params = new URLSearchParams({
        q: query.q,
        company: query.company,
        limit: String(query.limit),
      });
      return fetchJSON<BulletinBoard>(`/api/bulletin?${params}`, signal);
    },
    refetchInterval: 30_000,
    refetchIntervalInBackground: false,
    placeholderData: keepPreviousData,
    staleTime: 20_000,
    retry: 1,
  });
}

export function useBulletinThread(id: string) {
  return useQuery({
    queryKey: ["bulletin-thread", id],
    queryFn: ({ signal }) => {
      const params = new URLSearchParams({ id });
      return fetchJSON<BulletinThreadView>(
        `/api/bulletin/thread?${params}`,
        signal,
      );
    },
    enabled: Boolean(id),
    refetchInterval: 30_000,
    refetchIntervalInBackground: false,
    placeholderData: keepPreviousData,
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
  const recorded = new Map<string, ShipDetail>(
    ships.map((item) => [
      item.ship.id,
      shipSource(account, item.ship.id) === "ships"
        ? item
        : (details[item.ship.id] ?? item),
    ]),
  );
  for (const loss of account?.ship_losses ?? []) {
    const detail =
      recorded.get(loss.ship_id) ??
      (shipSource(account, loss.ship_id) === "ships"
        ? undefined
        : details[loss.ship_id]);
    if (
      loss.recovered_at ||
      (loss.fate === "missing" && detail?.ship.status === "sunk")
    ) {
      if (detail) {
        recorded.set(
          loss.ship_id,
          detail.ship.status === "sunk" ? detail : { ...detail, loss },
        );
      }
      continue;
    }
    if (detail) {
      const observed = Date.parse(
        metadata(account, shipSource(account, loss.ship_id)).updated_at ?? "",
      );
      const superseded =
        detail.ship.status !== "sunk" &&
        (!Number.isFinite(observed) ||
          observed <= Date.parse(loss.observed_at));
      const retained = { ...detail, loss };
      if (
        detail.ship.status === "sunk" &&
        (!detail.ship.fate || detail.ship.fate === "unknown")
      ) {
        retained.ship = { ...detail.ship, fate: loss.fate };
      }
      if (superseded) {
        retained.ship = {
          ...detail.ship,
          status: loss.status,
          fate: loss.fate,
          current_port_id: null,
          current_leg_idx: null,
          turnaround_unlocks_at: null,
          dock_queued_at: null,
          leg_departs_at: null,
          leg_arrives_at: null,
          berthed_at: null,
          convoy_id: null,
          stores_provisions_units: null,
        };
        retained.cargo_used_tons = null;
        retained.cargo_manifest = null;
        retained.stores = null;
        retained.route_legs = [];
        retained.voyage = null;
      }
      recorded.set(loss.ship_id, retained);
    } else {
      recorded.set(loss.ship_id, {
        ship: {
          id: loss.ship_id,
          company_id: account?.company_id ?? "",
          name: loss.ship_name,
          ship_class: loss.ship_class,
          status: loss.status,
          fate: loss.fate,
          current_port_id: null,
          current_leg_idx: null,
          cannons_count: null,
          stores_provisions_units: null,
          updated_at: null,
        },
        loss,
        hold_capacity_tons: null,
        effective_hold_capacity_tons: null,
        cargo_used_tons: null,
        cargo_manifest: null,
        stores: null,
        route_legs: [],
        voyage: null,
      });
    }
  }
  return [...recorded.values()];
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
