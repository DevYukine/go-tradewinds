import { dataset, metadata, number, record, shipSource, text } from "./data";
import { count, humanize } from "./format";
import type {
  Account,
  Catalog,
  DashboardData,
  EngineHold,
  Leaderboard,
  ShipDetail,
  Vault,
} from "./types";

export type ObservationState =
  | "current"
  | "delayed"
  | "stale"
  | "incomplete"
  | "missing"
  | "unknown"
  | "error";

export interface DatasetObservation {
  state: ObservationState;
  timestamp: string | undefined;
  current: boolean;
}

export interface ShipOperation {
  observation: DatasetObservation;
  activity: string;
  route: string;
  cargo: string;
  starving: boolean;
  holds: EngineHold[];
  holdsCurrent: boolean;
}

export function datasetObservation(
  account: Account | undefined,
  name: string,
  now: number,
): DatasetObservation {
  const value = dataset(account, name);
  const meta = metadata(account, name);
  const updated = Date.parse(meta.updated_at ?? "");
  const upstream = meta.upstream_as_of
    ? Date.parse(meta.upstream_as_of)
    : updated;
  const observed = Math.min(updated, upstream);
  const dynamic =
    name === "_engine" ||
    name === "ships" ||
    name.startsWith("ship_details/") ||
    name === "clock" ||
    name === "vaults";
  let state: ObservationState;
  if (value === undefined) {
    state = "missing";
  } else if (meta.error) {
    state = "error";
  } else if (meta.complete === false) {
    state = "incomplete";
  } else if (meta.stale) {
    state = "stale";
  } else if (
    meta.complete !== true ||
    !Number.isFinite(observed) ||
    !now ||
    observed > now
  ) {
    state = "unknown";
  } else if (dynamic && now - observed >= 120_000) {
    state = "stale";
  } else if (dynamic && now - observed >= 30_000) {
    state = "delayed";
  } else {
    state = "current";
  }
  if (name === "_engine" && (state === "current" || state === "delayed")) {
    const engine = record(value);
    if (
      !text(engine.generation, "") ||
      engine.generation !== meta.generation ||
      !Number.isFinite(Date.parse(text(engine.observed_at, ""))) ||
      Date.parse(text(engine.observed_at, "")) !== updated
    ) {
      state = "unknown";
    }
  }
  return {
    state,
    timestamp: Number.isFinite(observed)
      ? new Date(observed).toISOString()
      : undefined,
    current: state === "current",
  };
}

export function observationLabel(state: ObservationState): string {
  switch (state) {
    case "current":
      return "Current records";
    case "delayed":
      return "Delayed records";
    case "stale":
      return "Stale records";
    case "incomplete":
      return "Incomplete records";
    case "missing":
      return "Not recorded";
    case "error":
      return "Recording failed; retained data";
    case "unknown":
      return "Freshness unknown";
  }
}

export function controllerStatus(account: Account, now: number) {
  const engine = record(dataset(account, "_engine"));
  const holds: EngineHold[] = Array.isArray(engine.holds)
    ? engine.holds.flatMap((value) => {
        const hold = record(value);
        return typeof hold.ship_id === "string" &&
          typeof hold.reason === "string" &&
          hold.reason.length > 0
          ? [{ ship_id: hold.ship_id, reason: hold.reason }]
          : [];
      })
    : [];
  const assessedShipIDs =
    Array.isArray(engine.assessed_ship_ids) &&
    engine.assessed_ship_ids.every(
      (id): id is string => typeof id === "string" && id.length > 0,
    )
      ? engine.assessed_ship_ids
      : undefined;
  const observation = datasetObservation(account, "_engine", now);
  const observedAt = text(engine.observed_at, "");
  const nextCheckAt = text(engine.next_check_at, "");
  return {
    mode: text(engine.mode, ""),
    observation,
    observedAt: Number.isFinite(Date.parse(observedAt))
      ? observedAt
      : undefined,
    nextCheckAt: Number.isFinite(Date.parse(nextCheckAt))
      ? nextCheckAt
      : undefined,
    holds,
    assessedShipIDs,
    holdsValid:
      Array.isArray(engine.holds) && holds.length === engine.holds.length,
    accountHolds: holds.filter((hold) => hold.ship_id === ""),
  };
}

export function shipOperation(
  account: Account,
  detail: ShipDetail,
  now: number,
): ShipOperation {
  const source = shipSource(account, detail.ship.id);
  const observation = datasetObservation(account, source, now);
  const shipGeneration = metadata(account, source).generation;
  const controllerGeneration = metadata(account, "_engine").generation;
  const controller = controllerStatus(account, now);
  const catalog = dataset<Catalog>(account, "catalog");
  const portName = (id: string) =>
    catalog?.ports.find((port) => port.id === id)?.name ?? humanize(id);
  const sailing = detail.ship.status === "sailing";
  const origin = detail.route_legs?.[0]?.from_node;
  const destination = detail.route_legs?.at(-1)?.to_node;
  const route = sailing
    ? origin && destination
      ? `${portName(origin)} to ${portName(destination)}`
      : destination
        ? `To ${portName(destination)}`
        : "Route not recorded"
    : detail.ship.current_port_id
      ? portName(detail.ship.current_port_id)
      : "Port not recorded";
  const manifest = detail.cargo_manifest;
  const cargo = manifest
    ? Object.entries(manifest)
        .filter(([, quantity]) => Number.isFinite(quantity) && quantity > 0)
        .map(
          ([commodity, quantity]) =>
            `${count(quantity)} t ${catalog?.commodities.find((item) => item.id === commodity)?.name ?? humanize(commodity)}`,
        )
        .join(", ") || "No trade cargo recorded"
    : "Cargo not recorded";
  return {
    observation,
    activity: humanize(detail.ship.status),
    route,
    cargo,
    starving: Boolean(detail.voyage?.legs?.some((leg) => leg.starving)),
    holds: controller.holds.filter((hold) => hold.ship_id === detail.ship.id),
    holdsCurrent:
      observation.current &&
      controller.observation.current &&
      controller.holdsValid &&
      controller.assessedShipIDs?.includes(detail.ship.id) === true &&
      Boolean(shipGeneration) &&
      shipGeneration === controllerGeneration,
  };
}

export function companyMoney(account: Account, dashboard: DashboardData) {
  const vaults = dataset<{ vaults: Vault[] }>(account, "vaults")?.vaults;
  const cashVaults = vaults?.filter((vault) => vault.account_type === "vault");
  const cashPence = cashVaults?.every((vault) =>
    Number.isFinite(vault.balance_pence),
  )
    ? cashVaults.reduce((sum, vault) => sum + vault.balance_pence, 0)
    : undefined;
  const settlements = (dashboard.settlements ?? []).filter(
    (item) => item.account_id === account.id,
  );
  const known = settlements.filter(
    (item) =>
      item.costs_known &&
      [
        item.proceeds_pence,
        item.acquisition_pence,
        item.fees_pence,
        item.provisions_pence,
        item.expenses_pence,
      ].every(Number.isFinite),
  );
  const settledNetPence = known.length
    ? known.reduce(
        (sum, item) =>
          sum +
          item.proceeds_pence -
          item.acquisition_pence -
          item.fees_pence -
          item.provisions_pence -
          item.expenses_pence,
        0,
      )
    : undefined;
  const leaderboard = dataset<Leaderboard>(account, "leaderboard");
  return {
    cashPence,
    settledNetPence,
    knownSettlements: known.length,
    unknownSettlements: settlements.length - known.length,
    netWorthPence:
      leaderboard?.you?.is_calling_company === true
        ? number(leaderboard.you.net_worth_pence)
        : undefined,
    leaderboard,
  };
}
