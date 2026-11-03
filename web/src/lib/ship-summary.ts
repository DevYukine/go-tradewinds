import {
  chartBlockingHolds,
  chartCurrentPlan,
  chartHoldLabel,
  latestShipDecisions,
  type ChartPlan,
} from "./chart";
import { dataset, metadata, number, record, shipSource } from "./data";
import { buildDecisionView } from "./decisions";
import { count, dateTime, humanize } from "./format";
import {
  catalogPlaceName,
  datasetObservation,
  shipLossNeedsAttention,
  shipOperation,
  type ShipCargoItem,
  type ShipOperation,
} from "./operations";
import type {
  Account,
  Catalog,
  Decision,
  PassengerAboardGroup,
  ShipDetail,
} from "./types";

export interface ShipWhy {
  label: "Why" | "Last decision";
  text: string;
  attention: boolean;
}

export interface ShipPassengers {
  count: number | undefined;
  groups: PassengerAboardGroup[] | undefined;
  text: string;
  note: string | undefined;
}

export interface ShipSummary {
  operation: ShipOperation;
  gone: boolean;
  catalog: Catalog | undefined;
  cargoItems: ShipCargoItem[] | undefined;
  cargo: string;
  passengers: ShipPassengers;
  why: ShipWhy;
  plan: ChartPlan | undefined;
  decision: Decision | undefined;
  action: string;
  crew: number | undefined;
  eta: string | undefined;
  stores: string;
}

export function passengerHold(detail: ShipDetail): number | undefined {
  // Live fleet responses nest this field; decoded historical snapshots flatten it.
  return (
    number(detail.ship.passenger_hold_tons) ??
    number(detail.passenger_hold_tons)
  );
}

export function shipPassengers(
  account: Account,
  detail: ShipDetail,
  now: number,
): ShipPassengers {
  const key = `passenger_aboard/${detail.ship.id}`;
  const raw = dataset<unknown>(account, key);
  const hold = passengerHold(detail);
  const observation = datasetObservation(account, key, now);
  const shipMeta = metadata(account, shipSource(account, detail.ship.id));
  const groupMeta = metadata(account, key);
  const groups =
    Array.isArray(raw) &&
    raw.every((value) => {
      const group = record(value);
      return (
        Number.isSafeInteger(group.size) &&
        (group.size as number) > 0 &&
        typeof group.dest_port_id === "string" &&
        group.dest_port_id.length > 0 &&
        typeof group.kind === "string" &&
        typeof group.id === "string"
      );
    })
      ? (raw as PassengerAboardGroup[])
      : undefined;
  const sameCapture =
    Boolean(shipMeta.generation) &&
    shipMeta.generation === groupMeta.generation;
  const groupsCurrent =
    groups !== undefined && observation.current && sameCapture;
  // A newer explicit empty hold wins over an old aboard capture after arrival.
  if (hold === 0 && (!groupsCurrent || groups?.length === 0)) {
    return {
      count: 0,
      groups: undefined,
      text: "Passengers 0",
      note: undefined,
    };
  }
  if (
    groups !== undefined &&
    (hold !== 0 || groupsCurrent) &&
    (groups.length > 0 || hold === undefined || hold === 0)
  ) {
    const total = groups.reduce((sum, group) => sum + group.size, 0);
    if (Number.isSafeInteger(total)) {
      const retained = !groupsCurrent;
      return {
        count: total,
        groups,
        text: `${retained ? "Last recorded passengers" : "Passengers"} ${count(total)}`,
        note: retained
          ? "Passenger groups are retained records; current count is unavailable."
          : undefined,
      };
    }
  }
  return {
    count: undefined,
    groups: undefined,
    text:
      hold !== undefined && hold > 0
        ? "Passengers recorded; count unavailable"
        : "Passengers not recorded",
    note: undefined,
  };
}

export function shortShipReason(reason: string): string {
  const value = reason.trim();
  if (!value) return "Reason not recorded.";
  if (
    /^no (?:profitable executable voyage|executable purchase-and-voyage alternative) from /i.test(
      value,
    )
  ) {
    return `${chartHoldLabel(value)}.`;
  }
  // Only retain a complete short sentence, never an arbitrary character slice.
  const sentence = value.match(/^[^;\n.!?]+[.!?](?:\s|$)/)?.[0].trim();
  if (sentence && sentence.length <= 90 && !/[\u2013\u2014]/.test(sentence)) {
    return sentence.charAt(0).toUpperCase() + sentence.slice(1);
  }
  return "Reason recorded; open details.";
}

export function shipSummary(
  account: Account,
  detail: ShipDetail,
  decisions: Decision[],
  now: number,
  fallbackCatalog?: Catalog,
): ShipSummary {
  const catalog = dataset<Catalog>(account, "catalog") ?? fallbackCatalog;
  const operation = shipOperation(account, detail, now, catalog);
  const impaired =
    !operation.observation.current && operation.observation.state !== "delayed";
  const blockers = operation.holdsCurrent
    ? chartBlockingHolds(operation.holds, detail.ship.status)
    : [];
  const gone = detail.ship.status === "sunk";
  const sailing = detail.ship.status === "sailing";
  const port = detail.ship.current_port_id;
  const target = detail.route_legs?.at(-1)?.to_node;
  const action = gone
    ? operation.activity
    : sailing
      ? target
        ? `Sailing to ${catalogPlaceName(catalog, target)}`
        : "Sailing; destination not recorded"
      : detail.ship.status === "berthed"
        ? port
          ? `Berthed in ${catalogPlaceName(catalog, port)}`
          : "Berthed; port not recorded"
        : detail.ship.status === "anchored_roadstead"
          ? port
            ? `Anchored off ${catalogPlaceName(catalog, port)}`
            : "Anchored; port not recorded"
          : `${operation.activity}${port ? ` in ${catalogPlaceName(catalog, port)}` : ""}`;
  const crew = catalog?.ship_classes.find(
    (shipClass) => shipClass.id === detail.ship.ship_class,
  )?.crew_required;
  const passengers = shipPassengers(account, detail, now);
  const plan = gone
    ? undefined
    : chartCurrentPlan(account, detail.ship.id, decisions);
  const decision = latestShipDecisions(
    decisions,
    account.id,
    detail.ship.id,
  ).find((item) => Number.isFinite(Date.parse(item.created_at)));
  let why: ShipWhy = {
    label: "Why",
    text: "Reason not recorded.",
    attention: false,
  };
  if (gone) {
    why = {
      label: "Why",
      text: detail.loss?.hazard_cause?.trim()
        ? `${humanize(detail.loss.hazard_cause.trim()).replace(/[.!?]+$/, "")}.`
        : "",
      attention: shipLossNeedsAttention(detail.ship.status, detail.ship.fate),
    };
  } else if (operation.starving || impaired) {
    const recording = impaired
      ? {
          current: "",
          delayed: "",
          stale: "Ship records are stale.",
          error: "Ship recording failed; showing retained data.",
          incomplete: "Ship records are incomplete.",
          missing: "Ship records are unavailable.",
          unknown: "Ship freshness is unknown.",
        }[operation.observation.state]
      : "";
    why = {
      label: "Why",
      text: [
        operation.starving
          ? "Stores run out in the recorded voyage estimate."
          : "",
        recording,
      ]
        .filter(Boolean)
        .join(" "),
      attention: true,
    };
  } else if (blockers.length) {
    why = {
      label: "Why",
      text: `${chartHoldLabel(blockers[0].reason)}.${blockers.length > 1 ? ` +${blockers.length - 1} other ${blockers.length === 2 ? "blocker" : "blockers"}` : ""}`,
      attention: true,
    };
  } else if (plan?.source === "mission") {
    if (
      plan.phase === "return" ||
      plan.phase === "return_depart" ||
      plan.phase === "return_sailing"
    ) {
      why.text = plan.origin
        ? `Returning to ${catalogPlaceName(catalog, plan.origin)}.`
        : "Return destination not recorded.";
    } else if (plan.phase === "settling") {
      why.text = "Reconciling the completed voyage.";
    } else if (plan.kind === "aborted" || plan.phase === "closing") {
      why.text = "Closing the previous plan.";
    } else if (plan.kind === "maintenance") {
      why.text = "Maintenance in progress.";
    } else {
      const kinds: Record<string, string> = {
        spot: "Trade",
        charter: "Charter",
        "spot+freight": "Trade and freight",
        "charter+freight": "Charter and freight",
        recovery: "Voyage recovery",
        freight: "Freight",
        passage: "Passage",
        relocation: "Repositioning",
      };
      const kind = Object.hasOwn(kinds, plan.kind)
        ? kinds[plan.kind]
        : humanize(plan.kind);
      const quantity = number(plan.quantity);
      const load =
        plan.commodity && quantity !== undefined && quantity > 0
          ? `${count(quantity)} t ${catalog?.commodities.find((commodity) => commodity.id === plan.commodity)?.name ?? humanize(plan.commodity)}`
          : "";
      why.text = plan.destination
        ? `${kind}${load ? ` ${load}` : ""} to ${catalogPlaceName(catalog, plan.destination)}.`
        : `${kind}${load ? ` ${load}` : " plan"}; destination not recorded.`;
    }
  } else if (operation.holdsCurrent && operation.holds.length) {
    why.text = `${chartHoldLabel(operation.holds[0].reason)}.`;
  } else if (decision) {
    const view = buildDecisionView(decision.details);
    why = {
      label: "Last decision",
      text: shortShipReason(decision.reason),
      attention: false,
    };
    if (why.text === "Reason recorded; open details.") {
      if (
        decision.kind.startsWith("rejected:") ||
        record(decision.details).accepted === false
      ) {
        why.text = "Route rejected; open details.";
      } else if (
        [
          "spot",
          "charter",
          "freight",
          "spot+freight",
          "charter+freight",
        ].includes(decision.kind) &&
        !view.notPriced &&
        view.expectedNetPence !== undefined &&
        view.expectedNetPence > 0
      ) {
        why.text = "Trade modeled profitable.";
      }
    }
  }
  const arrival = sailing ? detail.voyage?.estimated_arrival_at : undefined;
  const arrivalTime = Date.parse(arrival ?? "");
  const seaDays = number(detail.stores?.sea_days_remaining);
  const storesCount =
    seaDays !== undefined && seaDays > 0 && seaDays < 0.1
      ? "<0.1"
      : count(seaDays);
  return {
    operation,
    gone,
    catalog,
    cargoItems: operation.cargoItems,
    cargo: operation.cargo,
    passengers,
    why,
    plan,
    decision,
    action: gone || !impaired ? action : `Last recorded: ${action}`,
    crew:
      typeof crew === "number" && Number.isFinite(crew) && crew >= 0
        ? crew
        : undefined,
    eta: Number.isFinite(arrivalTime)
      ? arrivalTime <= now
        ? "Awaiting updated arrival"
        : `Est. ${dateTime(arrival)}`
      : undefined,
    stores:
      seaDays === undefined
        ? "Stores not recorded"
        : `Stores ${storesCount} sea ${storesCount === "1" ? "day" : "days"}`,
  };
}
