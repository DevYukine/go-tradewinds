import { dataset, metadata, number, record, shipSource, text } from "./data";
import { count, humanize } from "./format";
import type {
  Account,
  Catalog,
  DashboardData,
  EngineHold,
  Leaderboard,
  Settlement,
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

export interface ShipCargoItem {
  id: string;
  name: string;
  tons: number;
}

export interface ShipOperation {
  observation: DatasetObservation;
  activity: string;
  route: string;
  cargoItems: ShipCargoItem[] | undefined;
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
    name.startsWith("passenger_aboard/") ||
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

export function shipIsLoss(status: string, fate?: string | null): boolean {
  return (
    status === "sunk" &&
    (!fate ||
      fate === "unknown" ||
      fate === "lost_at_sea" ||
      fate === "captured")
  );
}

export function shipRecordKind(
  status: string,
  fate?: string | null,
): "loss" | "exit" | "absence" {
  return shipIsLoss(status, fate)
    ? "loss"
    : fate === "missing"
      ? "absence"
      : "exit";
}

export function shipStatusLabel(status: string, fate?: string | null): string {
  if (status !== "sunk") {
    return humanize(status);
  }
  switch (fate) {
    case "lost_at_sea":
    case "unknown":
      return "Sunk";
    case "captured":
      return "Captured";
    case "seized_smuggling":
      return "Seized for smuggling";
    case "seized_bailiff":
      return "Seized by the bailiff";
    case "seized_bankruptcy":
      return "Seized after bankruptcy";
    case "broken_up":
      return "Broken up";
    case "royal_navy":
      return "Commissioned to the Royal Navy";
    case "missing":
      return "Missing from fleet list";
    default:
      return fate ? "Gone" : "Sunk";
  }
}

export function shipLossNeedsAttention(
  status: string,
  fate?: string | null,
): boolean {
  return (
    shipIsLoss(status, fate) ||
    (status === "sunk" && Boolean(fate?.startsWith("seized_")))
  );
}

export function catalogPlaceName(
  catalog: Catalog | undefined,
  id: string | null | undefined,
): string {
  return (
    catalog?.ports.find((port) => port.id === id)?.name ??
    catalog?.sea_zones.find((zone) => zone.id === id)?.name ??
    humanize(id)
  );
}

export function shipLastLocation(
  detail: ShipDetail,
  catalog?: Catalog,
): string {
  const loss = detail.loss;
  if (loss) {
    if (loss.last_port_id) {
      return `Last afloat: ${catalogPlaceName(catalog, loss.last_port_id)}`;
    }
    return loss.last_sea_zone_id
      ? `At sea near ${catalogPlaceName(catalog, loss.last_sea_zone_id)} (approximate)`
      : "Last afloat location not recorded";
  }
  return detail.ship.current_port_id
    ? `Game-reported last port: ${catalogPlaceName(catalog, detail.ship.current_port_id)}`
    : "Last afloat location not recorded";
}
export function cargoEntries(
  manifest: ShipDetail["cargo_manifest"] | undefined,
): [string, number][] | undefined {
  if (!manifest || typeof manifest !== "object" || Array.isArray(manifest)) {
    return undefined;
  }
  const entries = Object.entries(manifest);
  return entries.every(
    ([, tons]) =>
      typeof tons === "number" && Number.isFinite(tons) && tons >= 0,
  )
    ? entries.filter(([, tons]) => tons > 0)
    : undefined;
}

export function shipCargo(
  detail: ShipDetail,
  catalog?: Catalog,
): { items: ShipCargoItem[] | undefined; text: string } {
  const gone = detail.ship.status === "sunk";
  const manifest = gone ? detail.loss?.cargo_manifest : detail.cargo_manifest;
  const items = cargoEntries(manifest)
    ?.sort(
      ([leftID, left], [rightID, right]) =>
        right - left || leftID.localeCompare(rightID),
    )
    .map(([id, tons]) => ({
      id,
      name:
        catalog?.commodities.find((commodity) => commodity.id === id)?.name ??
        humanize(id),
      tons,
    }));
  if (items === undefined) {
    return {
      items,
      text: gone ? "Last afloat cargo not recorded" : "Cargo not recorded",
    };
  }
  const compact = items
    .slice(0, 2)
    .map(({ name, tons }) => `${count(tons)} t ${name}`)
    .join(", ");
  const remaining = items.length - 2;
  const load = `${compact || "No trade cargo recorded"}${remaining > 0 ? ` +${remaining} ${remaining === 1 ? "good" : "goods"}` : ""}`;
  return {
    items,
    text: gone ? `Last afloat: ${load}` : items.length ? `Cargo ${load}` : load,
  };
}

export function shipOperation(
  account: Account,
  detail: ShipDetail,
  now: number,
  fallbackCatalog?: Catalog,
): ShipOperation {
  const source = shipSource(account, detail.ship.id);
  const observation = datasetObservation(account, source, now);
  const shipGeneration = metadata(account, source).generation;
  const controllerGeneration = metadata(account, "_engine").generation;
  const controller = controllerStatus(account, now);
  const catalog = dataset<Catalog>(account, "catalog") ?? fallbackCatalog;
  const portName = (id: string) => catalogPlaceName(catalog, id);
  const sailing = detail.ship.status === "sailing";
  const gone = detail.ship.status === "sunk";
  const origin = detail.route_legs?.[0]?.from_node;
  const destination = detail.route_legs?.at(-1)?.to_node;
  const route = gone
    ? shipLastLocation(detail, catalog)
    : sailing
      ? origin && destination
        ? `${portName(origin)} to ${portName(destination)}`
        : destination
          ? `To ${portName(destination)}`
          : "Route not recorded"
      : detail.ship.current_port_id
        ? portName(detail.ship.current_port_id)
        : "Port not recorded";
  const cargo = shipCargo(detail, catalog);
  return {
    observation,
    activity: shipStatusLabel(detail.ship.status, detail.ship.fate),
    route,
    cargoItems: cargo.items,
    cargo: cargo.text,
    starving:
      !gone && Boolean(detail.voyage?.legs?.some((leg) => leg.starving)),
    holds: controller.holds.filter((hold) => hold.ship_id === detail.ship.id),
    holdsCurrent:
      !gone &&
      (observation.current || observation.state === "delayed") &&
      (controller.observation.current ||
        controller.observation.state === "delayed") &&
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
    (item): item is Settlement & { proceeds_pence: number } =>
      item.costs_known &&
      item.proceeds_unknown !== true &&
      item.proceeds_pence !== null &&
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
