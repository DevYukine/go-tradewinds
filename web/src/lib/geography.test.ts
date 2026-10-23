import { describe, expect, it } from "vitest";
import {
  PORT_COORDINATES,
  shipCoordinate,
  shipPosition,
  type ChartPosition,
} from "./geography";
import type { Clock, DatasetMeta, RouteLeg, ShipDetail } from "./types";

// Regression coverage for the chart position pipeline: ETA clamping, clock
// offset estimation, and freeze gating onto an unchanged voyage only.

const DEPART = Date.parse("2026-10-22T12:00:00.000Z");
const ARRIVE = DEPART + 600_000;
const SERVER = DEPART + 240_000;
const MIDLEG_CLOCK = iso(DEPART + 240_000);

const DOVER_CALAIS: RouteLeg = {
  from_node: "dover",
  to_node: "calais",
  distance_nm: 40,
  edge_class: "open_sea",
  is_sound_toll: false,
};

const CALAIS_LONDON: RouteLeg = {
  from_node: "calais",
  to_node: "london",
  distance_nm: 90,
  edge_class: "open_sea",
  is_sound_toll: false,
};

function iso(ms: number): string {
  return new Date(ms).toISOString();
}

function makeShip(
  ship: Partial<ShipDetail["ship"]> = {},
  routeLegs: RouteLeg[] = [DOVER_CALAIS],
): ShipDetail {
  return {
    ship: {
      id: "ship-lapras",
      company_id: "company-the-pokemon-pirates",
      name: "Lapras",
      ship_class: "sloop",
      status: "sailing",
      current_port_id: null,
      current_leg_idx: 0,
      cannons_count: 4,
      stores_provisions_units: 12,
      leg_departs_at: iso(DEPART),
      leg_arrives_at: iso(ARRIVE),
      updated_at: iso(SERVER),
      ...ship,
    },
    hold_capacity_tons: 120,
    effective_hold_capacity_tons: 110,
    cargo_used_tons: 0,
    cargo_manifest: {},
    stores: { provisions_tons: 2, sea_days_remaining: 30 },
    route_legs: routeLegs,
    voyage: null,
  };
}

function meta(overrides: DatasetMeta = {}): DatasetMeta {
  return { updated_at: iso(SERVER), complete: true, ...overrides };
}

function makeClock(realUtc: string, paused = false): Clock {
  return {
    real_utc: realUtc,
    season_number: 1,
    unlocked_phase: 2,
    global_health_bps: 0,
    active_player_volume_24h: 12,
    paused,
    season_planned_end_at: null,
    calendar: {
      year: 1702,
      month: 10,
      day: 22,
      hour: 12,
      minute: 0,
      season: "autumn",
      time_scale: 1,
      total_game_hours: 0,
    },
  };
}

function liveEstimate(detail: ShipDetail): ChartPosition {
  const position = shipPosition(
    detail,
    meta(),
    makeClock(MIDLEG_CLOCK),
    meta(),
    SERVER,
  );
  if (!position) {
    throw new Error("fixture voyage produced no live estimate");
  }
  return position;
}

describe("shipCoordinate ETA clamping", () => {
  const sailing = makeShip();

  it("holds at the origin before and exactly at departure", () => {
    expect(shipCoordinate(sailing, DEPART - 60_000)).toEqual(
      PORT_COORDINATES.dover,
    );
    expect(shipCoordinate(sailing, DEPART)).toEqual(PORT_COORDINATES.dover);
  });

  it("interpolates midway along the leg", () => {
    const midpoint = shipCoordinate(sailing, DEPART + 300_000);
    expect(midpoint?.latitude).toBeCloseTo(51.0455, 10);
    expect(midpoint?.longitude).toBeCloseTo(1.5915, 10);
  });

  it("clamps to the destination at and after the scheduled arrival", () => {
    expect(shipCoordinate(sailing, ARRIVE)).toEqual(PORT_COORDINATES.calais);
    expect(shipCoordinate(sailing, ARRIVE + 900_000)).toEqual(
      PORT_COORDINATES.calais,
    );
  });

  it("rejects non-finite estimate times", () => {
    expect(shipCoordinate(sailing, Number.NaN)).toBeUndefined();
  });

  it("yields nothing for a sunk ship", () => {
    const sunk = makeShip({ status: "sunk" });
    expect(shipCoordinate(sunk, DEPART + 300_000)).toBeUndefined();
  });

  it("resolves a moored ship to its recorded port", () => {
    const berthed = makeShip({
      status: "berthed",
      current_port_id: "portsmouth",
    });
    expect(shipCoordinate(berthed, Number.NaN)).toEqual(
      PORT_COORDINATES.portsmouth,
    );
  });

  it("yields nothing when the recorded port is unknown", () => {
    const lost = makeShip({ status: "berthed", current_port_id: "atlantis" });
    expect(shipCoordinate(lost, Number.NaN)).toBeUndefined();
  });

  it("yields nothing when a leg endpoint is unknown", () => {
    const offChart = makeShip({}, [{ ...DOVER_CALAIS, from_node: "atlantis" }]);
    expect(shipCoordinate(offChart, DEPART + 300_000)).toBeUndefined();
  });

  it("yields nothing when leg timestamps are unusable", () => {
    expect(
      shipCoordinate(makeShip({ leg_departs_at: "" }), DEPART + 300_000),
    ).toBeUndefined();
    expect(
      shipCoordinate(
        makeShip({ leg_arrives_at: iso(DEPART) }),
        DEPART + 300_000,
      ),
    ).toBeUndefined();
    expect(
      shipCoordinate(
        makeShip({ leg_arrives_at: iso(DEPART - 60_000) }),
        DEPART + 300_000,
      ),
    ).toBeUndefined();
  });
});

describe("shipPosition", () => {
  it("extrapolates the recorded clock forward by its observation lag", () => {
    const position = shipPosition(
      makeShip(),
      meta(),
      makeClock(MIDLEG_CLOCK),
      meta({ updated_at: iso(SERVER - 10_000) }),
      SERVER,
    );
    expect(position?.kind).toBe("estimated");
    expect(position?.coordinate.latitude).toBeCloseTo(51.05875, 8);
    expect(position?.coordinate.longitude).toBeCloseTo(1.5475833, 6);
  });

  it("does not extrapolate a clock observed in the future", () => {
    const position = shipPosition(
      makeShip(),
      meta(),
      makeClock(MIDLEG_CLOCK),
      meta({ updated_at: iso(SERVER + 30_000) }),
      SERVER,
    );
    expect(position?.kind).toBe("estimated");
    expect(position?.coordinate.latitude).toBeCloseTo(51.0614, 8);
    expect(position?.coordinate.longitude).toBeCloseTo(1.5388, 8);
  });

  it("marks the endpoint awaiting while the ship still records as sailing", () => {
    const position = shipPosition(
      makeShip(),
      meta(),
      makeClock(iso(ARRIVE)),
      meta(),
      SERVER,
    );
    expect(position?.kind).toBe("awaiting");
    expect(position?.coordinate).toEqual(PORT_COORDINATES.calais);
  });

  it("confirms arrival only from a recorded port position", () => {
    const position = shipPosition(
      makeShip({ status: "berthed", current_port_id: "calais" }),
      meta(),
      makeClock(iso(ARRIVE)),
      meta(),
      SERVER,
    );
    expect(position?.kind).toBe("recorded");
    expect(position?.coordinate).toEqual(PORT_COORDINATES.calais);
    expect(position?.voyageKey).toBe("");
  });

  it("freezes the last estimate while the world is paused on a matching voyage", () => {
    const detail = makeShip();
    const previous = liveEstimate(detail);
    expect(previous.kind).toBe("estimated");
    const position = shipPosition(
      detail,
      meta(),
      makeClock(MIDLEG_CLOCK, true),
      meta(),
      SERVER,
      previous,
    );
    expect(position?.kind).toBe("frozen");
    expect(position?.coordinate).toEqual(previous.coordinate);
    expect(position?.voyageKey).toBe(previous.voyageKey);
  });

  it("yields nothing while paused without a prior estimate", () => {
    const position = shipPosition(
      makeShip(),
      meta(),
      makeClock(MIDLEG_CLOCK, true),
      meta(),
      SERVER,
    );
    expect(position).toBeUndefined();
  });

  it("freezes on a stale-flagged ship recording with a matching voyage", () => {
    const detail = makeShip();
    const previous = liveEstimate(detail);
    const position = shipPosition(
      detail,
      meta({ stale: true }),
      makeClock(MIDLEG_CLOCK),
      meta(),
      SERVER,
      previous,
    );
    expect(position?.kind).toBe("frozen");
    expect(position?.coordinate).toEqual(previous.coordinate);
  });

  it("freezes when the ship recording ages past the freshness window", () => {
    const detail = makeShip();
    const previous = liveEstimate(detail);
    const position = shipPosition(
      detail,
      meta({ updated_at: iso(SERVER - 121_000) }),
      makeClock(MIDLEG_CLOCK),
      meta(),
      SERVER,
      previous,
    );
    expect(position?.kind).toBe("frozen");
  });

  it("freezes when the recorded clock ages past the freshness window", () => {
    const detail = makeShip();
    const previous = liveEstimate(detail);
    const position = shipPosition(
      detail,
      meta(),
      makeClock(iso(SERVER - 120_000)),
      meta(),
      SERVER,
      previous,
    );
    expect(position?.kind).toBe("frozen");
  });

  it("never reuses a prior estimate after the voyage changes", () => {
    const previous = liveEstimate(makeShip());
    const advanced = makeShip({ current_leg_idx: 1 }, [
      DOVER_CALAIS,
      CALAIS_LONDON,
    ]);
    const position = shipPosition(
      advanced,
      meta(),
      makeClock(MIDLEG_CLOCK, true),
      meta(),
      SERVER,
      previous,
    );
    expect(position).toBeUndefined();
  });

  it("never reuses a recorded port position as a frozen estimate", () => {
    const recorded = shipPosition(
      makeShip({ status: "berthed", current_port_id: "calais" }),
      meta(),
      makeClock(MIDLEG_CLOCK, true),
      meta(),
      SERVER,
    );
    expect(recorded?.voyageKey).toBe("");
    const position = shipPosition(
      makeShip(),
      meta(),
      makeClock(MIDLEG_CLOCK, true),
      meta(),
      SERVER,
      recorded,
    );
    expect(position).toBeUndefined();
  });

  it("yields nothing for a sunk ship regardless of the clock", () => {
    const position = shipPosition(
      makeShip({ status: "sunk" }),
      meta(),
      makeClock(MIDLEG_CLOCK),
      meta(),
      SERVER,
    );
    expect(position).toBeUndefined();
  });

  it("records port positions even when the world clock is unusable", () => {
    const position = shipPosition(
      makeShip({ status: "berthed", current_port_id: "calais" }),
      meta(),
      makeClock(MIDLEG_CLOCK, true),
      meta({ stale: true }),
      SERVER,
    );
    expect(position?.kind).toBe("recorded");
    expect(position?.coordinate).toEqual(PORT_COORDINATES.calais);
  });
});
