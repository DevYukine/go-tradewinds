import {
  PORT_COORDINATES,
  estimatedChartTime,
  nodeCoordinate,
  shipPosition,
  type ChartPosition,
  type Coordinate,
} from "./geography";
import { fleet, number, record, text } from "./data";
import { buildDecisionView } from "./decisions";
import {
  shipIsLoss,
  shipRecordKind,
  shipStatusLabel,
  type ShipOperation,
} from "./operations";
import { humanize } from "./format";
import type {
  Account,
  Catalog,
  Clock,
  DatasetMeta,
  Decision,
  EngineHold,
  EngineWarning,
  ShipDetail,
  WarningReport,
} from "./types";

export interface ChartSegment {
  from: Coordinate;
  to: Coordinate;
  index: number;
}

export interface ChartSelection {
  shipID: string;
  accountID: string;
}

export function resolveChartDeepLink(
  accounts: readonly Account[],
  search: string,
): ChartSelection | undefined {
  const params = new URLSearchParams(search);
  const shipID = params.get("ship");
  const accountID = params.get("account");
  if (!shipID) return undefined;
  const matches = accounts.filter(
    (account) =>
      (!accountID || account.id === accountID) &&
      fleet(account).some((detail) => detail.ship.id === shipID),
  );
  return matches.length === 1
    ? { shipID, accountID: matches[0].id }
    : undefined;
}

export function chartSelectionURL(
  href: string,
  selection?: ChartSelection,
): string {
  const url = new URL(href);
  if (selection?.shipID && selection.accountID) {
    url.searchParams.set("ship", selection.shipID);
    url.searchParams.set("account", selection.accountID);
  } else {
    url.searchParams.delete("ship");
    url.searchParams.delete("account");
  }
  return url.toString();
}

interface ChartRosterEntry {
  key: string;
  account: Pick<Account, "label">;
  company: string;
  detail: ShipDetail;
  operation: Pick<ShipOperation, "route" | "activity" | "cargo" | "cargoItems">;
}

export function chartLossPort(
  detail: ShipDetail,
): { id: string; kind: "observed-port" | "game-port" } | undefined {
  if (detail.ship.status !== "sunk") return undefined;
  const loss = detail.loss;
  if (loss) {
    return loss.last_port_id
      ? { id: loss.last_port_id, kind: "observed-port" }
      : undefined;
  }
  return detail.ship.current_port_id
    ? { id: detail.ship.current_port_id, kind: "game-port" }
    : undefined;
}

export function chartPortActivity(
  vessels: readonly { detail: ShipDetail }[],
): Map<string, number> {
  const counts = new Map<string, number>();
  for (const { detail } of vessels) {
    if (detail.ship.status === "sunk") continue;
    const nodes =
      detail.ship.status === "sailing"
        ? [
            detail.route_legs?.[0]?.from_node,
            detail.route_legs?.at(-1)?.to_node,
          ]
        : [detail.ship.current_port_id];
    for (const id of new Set(nodes))
      if (id) counts.set(id, (counts.get(id) ?? 0) + 1);
  }
  return counts;
}

export function chartRoster<T extends ChartRosterEntry>(
  vessels: readonly T[],
  search: string,
  stateFilter: string,
  sort: string,
  catalog?: Catalog,
): T[] {
  const term = search.trim().toLocaleLowerCase();
  const state = (row: T) =>
    shipStatusLabel(row.detail.ship.status, row.detail.ship.fate);
  const arrival = (row: T) => {
    const eta = voyageETA(row.detail);
    return eta !== undefined && Number.isFinite(eta) ? eta : Infinity;
  };
  const fill = (row: T) => {
    const capacity = row.detail.effective_hold_capacity_tons;
    const used = row.detail.cargo_used_tons;
    return typeof capacity === "number" &&
      Number.isFinite(capacity) &&
      capacity > 0 &&
      typeof used === "number" &&
      Number.isFinite(used) &&
      used >= 0
      ? used / capacity
      : -1;
  };
  return vessels
    .filter((row) => {
      if (
        stateFilter &&
        row.detail.ship.status !== stateFilter &&
        state(row) !== stateFilter
      ) {
        return false;
      }
      if (!term) return true;
      const port = chartLossPort(row.detail);
      return [
        row.detail.ship.name,
        state(row),
        row.detail.ship.status === "sunk"
          ? `${shipRecordKind(row.detail.ship.status, row.detail.ship.fate)} ${row.detail.ship.fate ?? ""}`
          : "",
        row.company,
        row.account.label,
        row.operation.route,
        row.operation.activity,
        row.operation.cargo,
        ...(row.operation.cargoItems?.map(({ name }) => name) ?? []),
        port?.id,
        port ? humanize(port.id) : "",
        port ? catalog?.ports.find(({ id }) => id === port.id)?.name : "",
      ]
        .join(" ")
        .toLocaleLowerCase()
        .includes(term);
    })
    .sort((left, right) => {
      let result = 0;
      if (sort === "arrival") result = arrival(left) - arrival(right);
      else if (sort === "state")
        result = state(left).localeCompare(state(right));
      else if (sort === "cargo") result = fill(right) - fill(left);
      return (
        (Number.isNaN(result) ? 0 : result) ||
        left.detail.ship.name.localeCompare(right.detail.ship.name) ||
        left.key.localeCompare(right.key)
      );
    });
}

export function chartWarningAssessment(
  report: WarningReport | undefined,
  accountID: string,
  queryStatus: "pending" | "error" | "success",
  holdsCurrent: boolean,
): { status: "assessed" | "unassessed" | "unavailable"; message?: string } {
  if (queryStatus === "error") {
    return {
      status: "unavailable",
      message: report
        ? "Ship warnings unavailable. Last recorded warnings are retained."
        : "Ship warnings unavailable.",
    };
  }
  if (queryStatus === "pending" || !report) {
    return { status: "unassessed", message: "Ship warnings not assessed yet." };
  }
  if (report.unmonitored.some((account) => account.account_id === accountID)) {
    return {
      status: "unassessed",
      message: "Ship warnings are not monitored for this account.",
    };
  }
  const assessment = report.accounts.find(
    (account) => account.account_id === accountID,
  );
  if (!assessment?.available || assessment.error_code) {
    return { status: "unavailable", message: "Ship warnings unavailable." };
  }
  if (
    !holdsCurrent ||
    assessment.suppressed.some(
      (code) => code === "hold_stuck" || code === "upkeep_seizure",
    )
  ) {
    return {
      status: "unassessed",
      message: "Some ship warnings could not be assessed.",
    };
  }
  return { status: "assessed" };
}

export function latestShipDecisions(
  decisions: Decision[],
  accountID: string,
  shipID: string,
): Decision[] {
  return decisions
    .filter(
      (decision) =>
        decision.account_id === accountID && decision.ship_id === shipID,
    )
    .sort(
      (left, right) =>
        (Date.parse(right.created_at) || 0) -
          (Date.parse(left.created_at) || 0) || right.id - left.id,
    )
    .slice(0, 3);
}

export interface ChartPlan {
  kind: string;
  phase?: string;
  origin: string;
  destination: string;
  commodity: string;
  quantity: number | undefined;
  contract_id?: string;
  expectedNetPence: number | undefined;
  updatedAt: string;
  source: "mission" | "decision";
  rejected: boolean;
}

export function chartCurrentPlan(
  account: Account,
  shipID: string,
  decisions: Decision[],
): ChartPlan | undefined {
  if (account.missions_unavailable) return undefined;
  const mission = account.missions?.find(
    (item) => item.ship_id === shipID && item.plan.kind,
  );
  if (mission) {
    // Durable plans have no acceptance flag; zero-value Go economics alone
    // do not establish that the mission was priced.
    const view = buildDecisionView({ ...mission.plan, accepted: false });
    return {
      ...mission.plan,
      phase: mission.phase,
      expectedNetPence: view.notPriced ? undefined : view.expectedNetPence,
      updatedAt: mission.updated_at,
      source: "mission" as const,
      rejected: false,
    };
  }
  const decision = latestShipDecisions(decisions, account.id, shipID)[0];
  if (!decision) return undefined;
  const details = record(decision.details);
  const view = buildDecisionView(details);
  return {
    kind: text(details.kind, decision.kind.replace(/^rejected:/, "")),
    origin: view.origin,
    destination: view.destination,
    commodity: view.commodity,
    quantity: number(details.quantity),
    contract_id: text(details.contract_id, ""),
    expectedNetPence: view.notPriced ? undefined : view.expectedNetPence,
    updatedAt: decision.created_at,
    source: "decision" as const,
    rejected:
      details.accepted === false || decision.kind.startsWith("rejected:"),
  };
}

const sailingWaitLabels: Record<string, string> = {
  "actual fleet confirms voyage in flight; waiting for authoritative arrival":
    "Waiting for arrival",
  "authoritative voyage still in flight": "Waiting for arrival",
  "waiting for authoritative arrival": "Waiting for arrival",
  "sourcing relocation waits for authoritative source arrival":
    "Repositioning to inspect a market",
  "passenger passage in flight; waiting for fare receipts":
    "Carrying passengers to their destination",
};

const holdLabels: [prefix: string, label: string, blocking: boolean][] = [
  ["no profitable executable voyage from ", "No profitable route found", false],
  [
    "no executable purchase-and-voyage alternative from ",
    "No executable route found",
    false,
  ],
  [
    "berth-only operation waits for authoritative docking;",
    "Waiting for a berth",
    false,
  ],
  ["observe mode:", "Observation only; no actions are being sent", false],
  [
    "cannot replenish stores without spending protected regional bills reserve",
    "Funding needed",
    true,
  ],
  [
    "cargo purchase requires fresh local depth and customs",
    "Fresh records needed",
    true,
  ],
  [
    "mission destination and actual fleet disagree;",
    "Plan and ship records disagree",
    true,
  ],
  [
    "in-flight ship has no authoritative route;",
    "Voyage destination is not recorded",
    true,
  ],
  [
    "passenger hold is occupied but no promises are available",
    "Passenger group details are unavailable",
    true,
  ],
  [
    "passengers aboard require authoritative group promises",
    "Waiting for passenger group records",
    true,
  ],
  [
    "passenger closure waits for authoritative boarded promises",
    "Waiting for passenger group records",
    true,
  ],
  [
    "passenger hold waits for authoritative boarded promises",
    "Waiting for passenger group records",
    true,
  ],
  [
    "passenger loading waits for turnaround unlock",
    "Waiting for turnaround",
    false,
  ],
  [
    "passenger repricing waits for twelve game hours",
    "Waiting for passenger demand",
    false,
  ],
  [
    "passenger ladder retains each price for twelve game hours",
    "Waiting for passenger demand",
    false,
  ],
  [
    "passenger promise permits a bounded wait",
    "Waiting for another passenger group",
    false,
  ],
];

export function chartBlockingHolds(
  holds: EngineHold[],
  status: string,
): EngineHold[] {
  return holds.filter(({ reason }) => {
    const normalized = reason.trim().toLowerCase();
    if (status === "sailing" && Object.hasOwn(sailingWaitLabels, normalized))
      return false;
    return (
      holdLabels.find(([prefix]) => normalized.startsWith(prefix))?.[2] !==
      false
    );
  });
}

export function chartHoldLabel(reason: string): string {
  const normalized = reason.trim().toLowerCase();
  if (Object.hasOwn(sailingWaitLabels, normalized)) {
    return sailingWaitLabels[normalized];
  }
  const authored = holdLabels.find(([prefix]) => normalized.startsWith(prefix));
  if (authored) return authored[1];
  if (/provision|starv/.test(normalized)) return "Provisions needed";
  if (/cash|fund|balance|credit/.test(normalized)) return "Funding needed";
  if (/cargo|hold capacity/.test(normalized)) return "Cargo blocked";
  if (/repair|damage/.test(normalized)) return "Repairs needed";
  if (/fresh|stale|observ|resync/.test(normalized))
    return "Fresh records needed";
  return "Action blocked";
}

export function chartShipWarnings(
  warnings: EngineWarning[],
  detail: ShipDetail,
): EngineWarning[] {
  return warnings.filter(
    (warning) =>
      (warning.code === "hold_stuck" || warning.code === "upkeep_seizure") &&
      (warning.names.ship_id
        ? warning.names.ship_id === detail.ship.id
        : warning.names.ship === detail.ship.name),
  );
}

export function splitChartRoute(
  segments: ChartSegment[],
  currentLegIndex: number | null,
  position?: ChartPosition,
): { wake: ChartSegment[]; remaining: ChartSegment[] } {
  const wake: ChartSegment[] = [];
  const remaining: ChartSegment[] = [];
  const coordinate = position?.coordinate;
  const usablePosition =
    coordinate &&
    Number.isFinite(coordinate.latitude) &&
    Number.isFinite(coordinate.longitude);

  for (const segment of segments) {
    if (currentLegIndex !== null && segment.index < currentLegIndex) {
      wake.push(segment);
    } else if (segment.index === currentLegIndex && usablePosition) {
      wake.push({ ...segment, to: coordinate });
      remaining.push({ ...segment, from: coordinate });
    } else {
      remaining.push(segment);
    }
  }
  return { wake, remaining };
}

export function voyageETA(detail: ShipDetail): number | undefined {
  if (detail.ship.status !== "sailing") {
    return undefined;
  }

  const voyageArrival = Date.parse(detail.voyage?.estimated_arrival_at ?? "");
  if (Number.isFinite(voyageArrival)) {
    return voyageArrival;
  }

  const finalIndex = (detail.route_legs?.length ?? 0) - 1;
  const finalLeg = detail.route_legs?.[finalIndex];
  if (!finalLeg) {
    return undefined;
  }
  const estimate = detail.voyage?.legs?.find(
    (leg) =>
      leg.leg_idx === finalIndex &&
      leg.from_node === finalLeg.from_node &&
      leg.to_node === finalLeg.to_node,
  );
  const finalLegArrival = Date.parse(estimate?.estimated_arrival_at ?? "");
  if (Number.isFinite(finalLegArrival)) {
    return finalLegArrival;
  }

  if (detail.ship.current_leg_idx === finalIndex) {
    const activeLegArrival = Date.parse(detail.ship.leg_arrives_at ?? "");
    if (Number.isFinite(activeLegArrival)) {
      return activeLegArrival;
    }
  }
  return undefined;
}

export function relativeETA(eta: number | undefined, now: number): string {
  if (eta === undefined || !Number.isFinite(eta) || !Number.isFinite(now)) {
    return "Arrival not recorded";
  }
  if (eta <= now) {
    return "Awaiting arrival recording";
  }

  const totalMinutes = Math.floor((eta - now) / 60_000);
  if (totalMinutes < 1) {
    return "under 1 min";
  }
  const hours = Math.floor(totalMinutes / 60);
  const minutes = totalMinutes % 60;
  return hours
    ? `in ${hours} h${minutes ? ` ${minutes} min` : ""}`
    : `in ${minutes} min`;
}
export function chartETA(
  detail: ShipDetail,
  clock: Clock | undefined,
  clockMeta: DatasetMeta,
  shipMeta: DatasetMeta,
  serverUTC: number,
): string {
  const eta = voyageETA(detail);
  if (eta === undefined) return "Arrival not recorded";
  const time = estimatedChartTime(clock, clockMeta, serverUTC, shipMeta);
  if (time.reason)
    return time.reason === "world paused"
      ? "Countdown paused"
      : "Countdown unavailable";
  return relativeETA(eta, time.utc ?? Number.NaN);
}

export function chartUnavailableReason(
  detail: ShipDetail,
  clock?: Clock,
): string {
  const ship = detail.ship;
  if (ship.status === "sunk") {
    const kind = shipRecordKind(ship.status, ship.fate);
    return `${humanize(kind)} location not recorded; no mapped last observed port is available. At-sea observations remain unplaced.`;
  }
  if (ship.status !== "sailing")
    return ship.current_port_id
      ? `No chart coordinates for ${ship.current_port_id.replaceAll("_", " ")}.`
      : "Current port not recorded.";
  const leg =
    ship.current_leg_idx === null
      ? undefined
      : detail.route_legs?.[ship.current_leg_idx];
  if (!leg) return "Current route leg not recorded.";
  const unmapped = [leg.from_node, leg.to_node].filter(
    (node) => !nodeCoordinate(node),
  );
  if (unmapped.length)
    return `No chart coordinates for ${unmapped.map((node) => node.replaceAll("_", " ")).join(", ")}.`;
  if (clock?.paused) return "World paused; no earlier estimate is available.";
  return "Position unavailable; ship or clock recording is incomplete, stale or missing.";
}

export function chartPosition(
  detail: ShipDetail,
  shipMeta: DatasetMeta,
  clock: Clock | undefined,
  clockMeta: DatasetMeta,
  serverUTC: number,
  previous?: ChartPosition,
): ChartPosition | undefined {
  if (detail.ship.status === "sunk") {
    const port = chartLossPort(detail);
    if (!port) return undefined;
    const coordinate = PORT_COORDINATES[port.id];
    return coordinate
      ? {
          coordinate,
          voyageKey: "",
          kind: "recorded",
          label: `${port.kind === "observed-port" ? "Last observed port" : "Game-reported last port"}; ${shipIsLoss(detail.ship.status, detail.ship.fate) ? "loss" : "exit or absence"} location not recorded`,
        }
      : undefined;
  }
  return shipPosition(detail, shipMeta, clock, clockMeta, serverUTC, previous);
}
