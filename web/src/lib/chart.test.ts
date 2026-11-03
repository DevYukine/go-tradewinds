import { describe, expect, it } from "vitest";
import {
  chartBlockingHolds,
  chartCurrentPlan,
  chartHoldLabel,
  chartLossPort,
  chartPortActivity,
  chartShipWarnings,
  chartRoster,
  chartSelectionURL,
  chartWarningAssessment,
  resolveChartDeepLink,
  chartETA,
  chartUnavailableReason,
  latestShipDecisions,
  chartPosition,
  relativeETA,
  splitChartRoute,
  voyageETA,
} from "./chart";
import {
  PORT_COORDINATES,
  routeSegments,
  type ChartPosition,
} from "./geography";
import { fleet } from "./data";
import { shipOperation, type ShipOperation } from "./operations";
import type {
  Account,
  Catalog,
  EngineWarning,
  Clock,
  DatasetMeta,
  Decision,
  RouteLeg,
  ShipDetail,
  ShipLoss,
  WarningReport,
} from "./types";

const DEPART = Date.parse("2026-10-22T12:00:00.000Z");
const ARRIVE = DEPART + 600_000;
const NOW = DEPART + 240_000;
const FINAL_ARRIVAL = ARRIVE + 3_600_000;
const DOVER_CALAIS: RouteLeg = {
  from_node: "dover",
  to_node: "calais",
  distance_nm: 40,
  edge_class: "open_sea",
  is_sound_toll: false,
};
const CALAIS_LONDON: RouteLeg = {
  ...DOVER_CALAIS,
  from_node: "calais",
  to_node: "london",
};

function iso(time: number): string {
  return new Date(time).toISOString();
}

function makeShip(
  ship: Partial<ShipDetail["ship"]> = {},
  routeLegs: RouteLeg[] = [DOVER_CALAIS, CALAIS_LONDON],
): ShipDetail {
  return {
    ship: {
      id: "ship-lapras",
      company_id: "company-pokemon-pirates",
      name: "Lapras",
      ship_class: "sloop",
      status: "sailing",
      current_port_id: null,
      current_leg_idx: 0,
      cannons_count: 4,
      stores_provisions_units: 12,
      leg_departs_at: iso(DEPART),
      leg_arrives_at: iso(ARRIVE),
      updated_at: iso(NOW),
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

function makeLoss(overrides: Partial<ShipLoss> = {}): ShipLoss {
  return {
    account_id: "a",
    ship_id: "ship-lapras",
    ship_name: "Lapras",
    ship_class: "sloop",
    fate: "lost_at_sea",
    status: "sunk",
    observed_at: iso(NOW),
    last_observed_at: iso(DEPART),
    recovered_at: null,
    last_port_id: "dover",
    last_sea_zone_id: "dover_strait",
    voyage_origin_id: "london",
    voyage_destination_id: "calais",
    cargo_manifest: null,
    stores: null,
    hazard_cause: null,
    insurance_claim_paid_pence: null,
    ...overrides,
  };
}

function makeAccount(ships: ShipDetail[], losses: ShipLoss[] = []): Account {
  return {
    id: "a",
    label: "Yukine",
    company_id: "company-pokemon-pirates",
    enabled: true,
    created_at: iso(DEPART),
    snapshot: {
      ships,
      _meta: { ships: meta() },
    },
    ship_losses: losses,
  };
}

function makeVoyage(
  overrides: Partial<NonNullable<ShipDetail["voyage"]>> = {},
): NonNullable<ShipDetail["voyage"]> {
  return {
    current_leg_idx: 0,
    speed_centiknots: 600,
    estimated_arrival_at: iso(FINAL_ARRIVAL),
    legs: [DOVER_CALAIS, CALAIS_LONDON].map((leg, index) => ({
      ...leg,
      leg_idx: index,
      speed_centiknots: 600,
      estimated_arrival_at: iso(index === 0 ? ARRIVE : FINAL_ARRIVAL),
      starving: false,
    })),
    ...overrides,
  };
}

function meta(overrides: DatasetMeta = {}): DatasetMeta {
  return { updated_at: iso(NOW), complete: true, ...overrides };
}

function clock(paused = false, realUTC = NOW): Clock {
  return {
    real_utc: iso(realUTC),
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

function estimate(detail = makeShip()): ChartPosition {
  const position = chartPosition(detail, meta(), clock(), meta(), NOW);
  if (!position) {
    throw new Error("fixture must produce a chart position");
  }
  return position;
}

describe("splitChartRoute", () => {
  it("separates completed legs and splits the active segment at its supplied position", () => {
    const detail = makeShip({ current_leg_idx: 1 });
    const segments = routeSegments(detail);
    const position = estimate(detail);
    const result = splitChartRoute(segments, 1, position);
    expect(result.wake).toEqual([
      segments[0],
      { ...segments[1], to: position.coordinate },
    ]);
    expect(result.remaining).toEqual([
      { ...segments[1], from: position.coordinate },
    ]);
    expect(segments[1].from).toEqual(PORT_COORDINATES.calais);
    expect(segments[1].to).toEqual(PORT_COORDINATES.london);
  });

  it("keeps the current leg and all future legs remaining without a position", () => {
    const segments = routeSegments(makeShip());
    expect(splitChartRoute(segments, 0)).toEqual({
      wake: [],
      remaining: segments,
    });
    expect(splitChartRoute(segments, 1)).toEqual({
      wake: [segments[0]],
      remaining: [segments[1]],
    });
  });

  it("keeps every leg remaining when the recorded current leg is unknown", () => {
    const segments = routeSegments(makeShip());
    expect(splitChartRoute(segments, null, estimate())).toEqual({
      wake: [],
      remaining: segments,
    });
  });

  it("does not split with non-finite coordinates", () => {
    const segments = routeSegments(makeShip());
    const invalid = {
      ...estimate(),
      coordinate: { latitude: Number.NaN, longitude: Number.POSITIVE_INFINITY },
    };
    expect(splitChartRoute(segments, 0, invalid)).toEqual({
      wake: [],
      remaining: segments,
    });
  });

  it("preserves original leg indices across unknown endpoints without bridging gaps", () => {
    const detail = makeShip({}, [
      DOVER_CALAIS,
      { ...DOVER_CALAIS, to_node: "uncharted_water" },
      { ...CALAIS_LONDON, from_node: "uncharted_water" },
      CALAIS_LONDON,
    ]);
    const segments = routeSegments(detail);
    expect(segments.map((segment) => segment.index)).toEqual([0, 3]);
    expect(splitChartRoute(segments, 2, estimate())).toEqual({
      wake: [segments[0]],
      remaining: [segments[1]],
    });
  });

  it("returns no invented geometry when no segments are charted", () => {
    expect(splitChartRoute([], 0, estimate())).toEqual({
      wake: [],
      remaining: [],
    });
  });
});

describe("voyageETA", () => {
  it("prefers the final voyage estimate over current and final leg estimates", () => {
    const detail = makeShip();
    detail.voyage = makeVoyage({
      estimated_arrival_at: iso(FINAL_ARRIVAL + 60_000),
    });
    expect(voyageETA(detail)).toBe(FINAL_ARRIVAL + 60_000);
  });

  it("falls back to a matching final route leg, independent of estimate array order", () => {
    const detail = makeShip();
    const voyage = makeVoyage({ estimated_arrival_at: "invalid" });
    detail.voyage = { ...voyage, legs: [...voyage.legs].reverse() };
    expect(voyageETA(detail)).toBe(FINAL_ARRIVAL);
  });

  it("rejects an estimate whose index or endpoints do not match the final route leg", () => {
    const detail = makeShip();
    const voyage = makeVoyage({ estimated_arrival_at: "" });
    detail.voyage = {
      ...voyage,
      legs: [{ ...voyage.legs[1], from_node: "dover" }],
    };
    expect(voyageETA(detail)).toBeUndefined();
    detail.voyage.legs = [{ ...voyage.legs[1], leg_idx: 7 }];
    expect(voyageETA(detail)).toBeUndefined();
  });

  it("uses the active leg arrival only when that leg really is the final route leg", () => {
    expect(voyageETA(makeShip())).toBeUndefined();
    expect(voyageETA(makeShip({ current_leg_idx: 1 }))).toBe(ARRIVE);
    expect(voyageETA(makeShip({ current_leg_idx: 2 }))).toBeUndefined();
    expect(voyageETA(makeShip({}, []))).toBeUndefined();
  });

  it("continues to the active final leg when both voyage estimates are invalid", () => {
    const detail = makeShip({ current_leg_idx: 1 });
    const voyage = makeVoyage({ estimated_arrival_at: "invalid" });
    detail.voyage = {
      ...voyage,
      legs: [{ ...voyage.legs[1], estimated_arrival_at: "invalid" }],
    };
    expect(voyageETA(detail)).toBe(ARRIVE);
  });

  it("rejects invalid active timestamps but preserves past estimates for honest overdue labeling", () => {
    expect(
      voyageETA(makeShip({ current_leg_idx: 1, leg_arrives_at: "invalid" })),
    ).toBeUndefined();
    const detail = makeShip();
    detail.voyage = makeVoyage({ estimated_arrival_at: iso(NOW - 60_000) });
    expect(voyageETA(detail)).toBe(NOW - 60_000);
    expect(relativeETA(voyageETA(detail), NOW)).toBe(
      "Awaiting arrival recording",
    );
  });

  it.each(["berthed", "sunk", "repairing"])(
    "has no voyage ETA for a ship recorded as %s",
    (status) => {
      const detail = makeShip({ status, current_leg_idx: 1 });
      detail.voyage = makeVoyage();
      expect(voyageETA(detail)).toBeUndefined();
    },
  );
});

describe("relativeETA", () => {
  it.each<[number | undefined, string]>([
    [undefined, "Arrival not recorded"],
    [Number.NaN, "Arrival not recorded"],
    [Number.POSITIVE_INFINITY, "Arrival not recorded"],
    [NOW - 1, "Awaiting arrival recording"],
    [NOW, "Awaiting arrival recording"],
    [NOW + 1, "under 1 min"],
    [NOW + 59_999, "under 1 min"],
    [NOW + 60_000, "in 1 min"],
    [NOW + 300_000, "in 5 min"],
    [NOW + 3_600_000, "in 1 h"],
    [NOW + (3 * 60 + 12) * 60_000, "in 3 h 12 min"],
  ])("formats %s without asserting a recorded arrival", (eta, expected) => {
    expect(relativeETA(eta, NOW)).toBe(expected);
  });

  it("cannot report a countdown without a usable current time", () => {
    expect(relativeETA(ARRIVE, Number.NaN)).toBe("Arrival not recorded");
  });
});

describe("chartPortActivity", () => {
  it("counts only active current ports and deduplicated sailing endpoints", () => {
    const account = makeAccount(
      [
        makeShip({
          id: "berthed",
          status: "berthed",
          current_port_id: "calais",
        }),
        makeShip({ id: "sailing" }),
        makeShip({ id: "round-trip" }, [{ ...DOVER_CALAIS, to_node: "dover" }]),
        makeShip({
          id: "anchored",
          status: "anchored_roadstead",
          current_port_id: "london",
        }),
        makeShip({
          id: "repairing",
          status: "repairing",
          current_port_id: "calais",
        }),
        makeShip({
          id: "unknown-port",
          status: "berthed",
          current_port_id: null,
        }),
        makeShip({ id: "unknown-route", current_port_id: "calais" }, []),
        makeShip({
          id: "game-sunk",
          status: "sunk",
          current_port_id: "calais",
        }),
        makeShip({
          id: "recovered",
          status: "berthed",
          current_port_id: "london",
        }),
      ],
      [
        makeLoss({ ship_id: "omitted-sunk" }),
        makeLoss({ ship_id: "recovered", recovered_at: iso(NOW) }),
      ],
    );
    const rows = fleet(account).map((detail) => ({ detail }));
    expect(chartPortActivity(rows)).toEqual(
      new Map([
        ["calais", 2],
        ["dover", 2],
        ["london", 3],
      ]),
    );
    expect(
      chartPortActivity(
        rows.filter((row) => row.detail.ship.status === "sunk"),
      ),
    ).toEqual(new Map());
    expect(chartPortActivity([])).toEqual(new Map());
  });
});

describe("chartPosition", () => {
  it("distinguishes estimates from recorded ports and scheduled endpoints", () => {
    expect(estimate().kind).toBe("estimated");
    const awaiting = chartPosition(
      makeShip(),
      meta(),
      clock(false, ARRIVE),
      meta(),
      NOW,
    );
    expect(awaiting?.kind).toBe("awaiting");
    expect(awaiting?.label).toContain("Arrival is not confirmed");
    const recorded = chartPosition(
      makeShip({ status: "berthed", current_port_id: "calais" }),
      meta(),
      undefined,
      {},
      NOW,
    );
    expect(recorded?.kind).toBe("recorded");
    expect(recorded?.coordinate).toEqual(PORT_COORDINATES.calais);
  });

  it("retains a game-omitted hull and maps its durable last observed port", () => {
    const account = makeAccount([], [makeLoss()]);
    const details = fleet(account);
    expect(details).toHaveLength(1);
    const [detail] = details;
    expect(detail.ship.status).toBe("sunk");
    expect(detail.ship.current_port_id).toBeNull();
    expect(detail.ship.current_leg_idx).toBeNull();
    expect(chartLossPort(detail)).toEqual({
      id: "dover",
      kind: "observed-port",
    });
    const position = chartPosition(
      detail,
      meta(),
      undefined,
      {},
      NOW,
      estimate(),
    );
    expect(position).toEqual({
      coordinate: PORT_COORDINATES.dover,
      voyageKey: "",
      kind: "recorded",
      label: "Last observed port; loss location not recorded",
    });
    expect(
      resolveChartDeepLink([account], "?ship=ship-lapras&account=a"),
    ).toEqual({
      shipID: "ship-lapras",
      accountID: "a",
    });
  });

  it("prefers durable port context over a sunk record's current port, voyage origin and estimate", () => {
    const account = makeAccount(
      [makeShip({ status: "sunk", current_port_id: "calais" })],
      [makeLoss()],
    );
    const [detail] = fleet(account);
    const previous = estimate();
    const position = chartPosition(
      detail,
      meta(),
      clock(),
      meta(),
      NOW,
      previous,
    );
    expect(position?.coordinate).toEqual(PORT_COORDINATES.dover);
    expect(position?.coordinate).not.toEqual(previous.coordinate);
    expect(position?.label).toBe(
      "Last observed port; loss location not recorded",
    );
  });

  it.each([true, false])(
    "leaves an at-sea observation unplaced despite a voyage origin and current port (omitted hull: %s)",
    (omitted) => {
      const account = makeAccount(
        omitted
          ? []
          : [makeShip({ status: "sunk", current_port_id: "calais" })],
        [makeLoss({ last_port_id: null })],
      );
      const [detail] = fleet(account);
      expect(chartLossPort(detail)).toBeUndefined();
      expect(
        chartPosition(detail, meta(), clock(), meta(), NOW, estimate()),
      ).toBeUndefined();
      expect(shipOperation(account, detail, NOW).route).toBe(
        "At sea near Dover strait (approximate)",
      );
      expect(chartUnavailableReason(detail)).toContain(
        "At-sea observations remain unplaced",
      );
    },
  );

  it.each([
    { last_port_id: "uncharted_port", voyage_origin_id: "london" },
    { last_port_id: "dover_strait", voyage_origin_id: "london" },
    { last_port_id: null, voyage_origin_id: "uncharted_port" },
    { last_port_id: null, voyage_origin_id: "dover_strait" },
  ])(
    "does not invent a wreck location from unmapped durable context %j",
    (context) => {
      const account = makeAccount(
        [makeShip({ status: "sunk", current_port_id: "calais" })],
        [makeLoss(context)],
      );
      const [detail] = fleet(account);
      expect(
        chartPosition(detail, meta(), clock(), meta(), NOW, estimate()),
      ).toBeUndefined();
      expect(chartUnavailableReason(detail, clock())).toContain(
        "Loss location not recorded",
      );
    },
  );

  it("does not use a sunk row's stale current port when the durable last observation was at sea", () => {
    const [detail] = fleet(
      makeAccount(
        [makeShip({ status: "sunk", current_port_id: "calais" })],
        [makeLoss({ last_port_id: null, voyage_origin_id: null })],
      ),
    );
    expect(chartLossPort(detail)).toBeUndefined();
    expect(
      chartPosition(detail, meta(), clock(), meta(), NOW, estimate()),
    ).toBeUndefined();
  });

  it("keeps an omitted hull unplaced when no port context was recorded", () => {
    const [detail] = fleet(
      makeAccount(
        [],
        [makeLoss({ last_port_id: null, voyage_origin_id: null })],
      ),
    );
    expect(chartLossPort(detail)).toBeUndefined();
    expect(
      chartPosition(detail, meta(), clock(), meta(), NOW, estimate()),
    ).toBeUndefined();
  });

  it("keeps a legacy sunk hull at its game-reported last port", () => {
    const [detail] = fleet(
      makeAccount([makeShip({ status: "sunk", current_port_id: "dover" })]),
    );
    expect(chartLossPort(detail)).toEqual({
      id: "dover",
      kind: "game-port",
    });
    expect(
      chartPosition(detail, meta(), clock(), meta(), NOW, estimate()),
    ).toEqual({
      coordinate: PORT_COORDINATES.dover,
      voyageKey: "",
      kind: "recorded",
      label: "Game-reported last port; loss location not recorded",
    });
  });

  it.each([null, "uncharted_port", "dover_strait"])(
    "does not invent a legacy wreck location for current port %s",
    (currentPort) => {
      const [detail] = fleet(
        makeAccount([
          makeShip({ status: "sunk", current_port_id: currentPort }),
        ]),
      );
      expect(chartLossPort(detail)).toEqual(
        currentPort === null
          ? undefined
          : { id: currentPort, kind: "game-port" },
      );
      expect(
        chartPosition(detail, meta(), clock(), meta(), NOW, estimate()),
      ).toBeUndefined();
    },
  );

  it.each(["sailing", "berthed"])(
    "keeps a recovered historical loss from replacing an afloat %s position",
    (status) => {
      const current = makeShip({
        status,
        current_port_id: status === "berthed" ? "calais" : null,
      });
      const [detail] = fleet(
        makeAccount(
          [current],
          [makeLoss({ fate: "missing", recovered_at: iso(NOW) })],
        ),
      );
      expect(detail.loss?.recovered_at).toBe(iso(NOW));
      expect(detail.ship.status).toBe(status);
      expect(chartLossPort(detail)).toBeUndefined();
      expect(chartPosition(detail, meta(), clock(), meta(), NOW)).toEqual(
        chartPosition(current, meta(), clock(), meta(), NOW),
      );
      expect(
        chartPosition(detail, meta(), clock(), meta(), NOW)?.coordinate,
      ).not.toEqual(PORT_COORDINATES.dover);
    },
  );

  it.each([null, "calais"])(
    "ignores a recovered loss's old ports when the hull is lost again at game port %s",
    (currentPort) => {
      const [detail] = fleet(
        makeAccount(
          [makeShip({ status: "sunk", current_port_id: currentPort })],
          [makeLoss({ recovered_at: iso(NOW) })],
        ),
      );
      expect(chartLossPort(detail)).toEqual(
        currentPort === null
          ? undefined
          : { id: currentPort, kind: "game-port" },
      );
      const position = chartPosition(
        detail,
        meta(),
        clock(),
        meta(),
        NOW,
        estimate(),
      );
      if (currentPort === null) {
        expect(position).toBeUndefined();
      } else {
        expect(position).toMatchObject({
          coordinate: PORT_COORDINATES.calais,
          label: "Game-reported last port; loss location not recorded",
        });
      }
    },
  );

  it.each(["paused", "stale ship", "stale clock"])(
    "freezes an unchanged voyage when %s after a long hidden-tab gap",
    (reason) => {
      const detail = makeShip();
      const previous = estimate(detail);
      const position = chartPosition(
        detail,
        meta({ stale: reason === "stale ship" }),
        clock(reason === "paused"),
        meta({ stale: reason === "stale clock" }),
        NOW + 3_600_000,
        previous,
      );
      expect(position?.kind).toBe("frozen");
      expect(position?.coordinate).toEqual(previous.coordinate);
      expect(position?.voyageKey).toBe(previous.voyageKey);
    },
  );

  it.each(["paused", "stale ship"])(
    "does not reuse an estimate for a changed voyage when %s",
    (reason) => {
      const previous = estimate();
      expect(
        chartPosition(
          makeShip({ current_leg_idx: 1 }),
          meta({ stale: reason === "stale ship" }),
          clock(reason === "paused"),
          meta(),
          NOW,
          previous,
        ),
      ).toBeUndefined();
      expect(
        chartPosition(
          makeShip({ leg_arrives_at: iso(ARRIVE + 60_000) }),
          meta({ stale: reason === "stale ship" }),
          clock(reason === "paused"),
          meta(),
          NOW,
          previous,
        ),
      ).toBeUndefined();
    },
  );

  it("has no paused estimate without an earlier usable position", () => {
    expect(
      chartPosition(makeShip(), meta(), clock(true), meta(), NOW),
    ).toBeUndefined();
  });
});

describe("chartETA", () => {
  it("uses the same recorded clock offset as the vessel estimate", () => {
    const detail = makeShip({ current_leg_idx: 1 });
    expect(
      chartETA(detail, clock(false, NOW + 60_000), meta(), meta(), NOW),
    ).toBe("in 5 min");
    expect(
      chartETA(
        detail,
        clock(false, NOW + 60_000),
        meta({ updated_at: iso(NOW - 60_000) }),
        meta(),
        NOW,
      ),
    ).toBe("in 4 min");
  });
  it("does not count down while paused, stale or missing a clock recording", () => {
    const detail = makeShip({ current_leg_idx: 1 });
    expect(chartETA(detail, clock(true), meta(), meta(), NOW)).toBe(
      "Countdown paused",
    );
    expect(chartETA(detail, clock(), meta({ stale: true }), meta(), NOW)).toBe(
      "Countdown unavailable",
    );
    expect(chartETA(detail, undefined, {}, meta(), NOW)).toBe(
      "Countdown unavailable",
    );
    expect(
      chartETA(detail, clock(), meta(), meta({ complete: false }), NOW),
    ).toBe("Countdown unavailable");
  });
  it("retains unknown final arrivals even when the world is paused", () => {
    expect(chartETA(makeShip(), clock(true), meta(), meta(), NOW)).toBe(
      "Arrival not recorded",
    );
  });
});

describe("chartUnavailableReason", () => {
  it("separates unmapped geometry from clock and recording failures", () => {
    expect(
      chartUnavailableReason(
        makeShip({}, [{ ...DOVER_CALAIS, to_node: "atlantis" }]),
        clock(),
      ),
    ).toContain("No chart coordinates for atlantis");
    expect(
      chartUnavailableReason(
        makeShip({ status: "berthed", current_port_id: "atlantis" }),
      ),
    ).toContain("No chart coordinates");
    expect(chartUnavailableReason(makeShip(), clock(true))).toContain(
      "World paused",
    );
    expect(chartUnavailableReason(makeShip({}, []))).toBe(
      "Current route leg not recorded.",
    );
    expect(
      chartUnavailableReason(makeShip({ current_leg_idx: null }), clock()),
    ).toBe("Current route leg not recorded.");
  });
  it("does not imply that a fresh clock reveals a wreck location", () => {
    expect(chartUnavailableReason(makeShip({ status: "sunk" }), clock())).toBe(
      "Loss location not recorded; no mapped last observed port is available. At-sea observations remain unplaced.",
    );
  });
});

describe("latestShipDecisions", () => {
  const decision = (
    id: number,
    created_at: string,
    overrides: Partial<Decision> = {},
  ): Decision => ({
    id,
    created_at,
    account_id: "a",
    ship_id: "s",
    kind: "spot",
    reason: "Recorded candidate",
    details: {},
    ...overrides,
  });
  it("keeps account-qualified identity, newest-first order, tie-breaks and sample bounds", () => {
    const input = [
      decision(1, iso(NOW)),
      decision(2, iso(NOW)),
      decision(3, iso(NOW + 1)),
      decision(4, iso(NOW + 2)),
      decision(8, iso(NOW + 3), { account_id: "other" }),
      decision(9, iso(NOW + 4), { ship_id: "other" }),
    ];
    expect(latestShipDecisions(input, "a", "s").map((item) => item.id)).toEqual(
      [4, 3, 2],
    );
    expect(input[0].id).toBe(1);
    expect(latestShipDecisions(input, "missing", "s")).toEqual([]);
  });
  it("sorts an unparseable decision timestamp last instead of hiding newer evidence", () => {
    expect(
      latestShipDecisions(
        [decision(2, ""), decision(1, iso(NOW))],
        "a",
        "s",
      ).map((item) => item.id),
    ).toEqual([1, 2]);
  });
});

describe("chart plan and ship-only attention", () => {
  const account: Account = {
    id: "a",
    label: "A",
    company_id: "c",
    enabled: true,
    created_at: iso(NOW),
    snapshot: {},
  };
  const decision: Decision = {
    id: 1,
    account_id: "a",
    ship_id: "ship-lapras",
    kind: "spot",
    reason: "Trade",
    created_at: iso(NOW),
    details: {
      kind: "spot",
      origin: "dover",
      destination: "calais",
      commodity: "grain",
      economics: { expected_net_pence: 1200 },
    },
  };

  it("prefers the durable mission and preserves a priced zero valuation", () => {
    expect(
      chartCurrentPlan(
        {
          ...account,
          missions: [
            {
              ship_id: "ship-lapras",
              phase: "sailing",
              updated_at: iso(DEPART),
              plan: {
                kind: "charter",
                origin: "london",
                destination: "hamburg",
                commodity: "salt",
                quantity: 20,
                contract_id: "charter-1",
                economics: { expected_net_pence: 0, basis: "destination" },
              },
            },
          ],
        },
        "ship-lapras",
        [decision],
      ),
    ).toMatchObject({
      source: "mission",
      kind: "charter",
      phase: "sailing",
      destination: "hamburg",
      expectedNetPence: 0,
      updatedAt: iso(DEPART),
    });
  });

  it("does not invent an absent valuation and scopes fallback decisions to the account", () => {
    expect(
      chartCurrentPlan(
        {
          ...account,
          missions: [
            {
              ship_id: "ship-lapras",
              phase: "sailing",
              updated_at: iso(DEPART),
              plan: {
                kind: "relocate",
                origin: "london",
                destination: "hamburg",
                commodity: "",
                quantity: 0,
                economics: {},
              },
            },
          ],
        },
        "ship-lapras",
        [decision],
      )?.expectedNetPence,
    ).toBeUndefined();
    expect(chartCurrentPlan(account, "ship-lapras", [decision])).toMatchObject({
      source: "decision",
      expectedNetPence: 1200,
    });
    expect(
      chartCurrentPlan({ ...account, id: "other" }, "ship-lapras", [decision]),
    ).toBeUndefined();
  });

  it("marks rejected fallback decisions and leaves unpriced plans unvalued", () => {
    expect(
      chartCurrentPlan(account, "ship-lapras", [
        {
          ...decision,
          kind: "rejected:spot",
          details: { accepted: false, economics: {} },
        },
      ]),
    ).toMatchObject({
      source: "decision",
      rejected: true,
      expectedNetPence: undefined,
    });
  });

  it("does not price zero-value mission economics or fall back when mission projection failed", () => {
    const mission = {
      ship_id: "ship-lapras",
      phase: "maintenance",
      updated_at: iso(NOW),
      plan: {
        kind: "maintenance",
        origin: "london",
        destination: "",
        commodity: "",
        quantity: 0,
        economics: { expected_net_pence: 0 },
      },
    };
    expect(
      chartCurrentPlan({ ...account, missions: [mission] }, "ship-lapras", [
        decision,
      ]),
    ).toMatchObject({
      source: "mission",
      expectedNetPence: undefined,
      destination: "",
    });
    expect(
      chartCurrentPlan(
        {
          ...account,
          missions: [mission],
          missions_unavailable: true,
        },
        "ship-lapras",
        [decision],
      ),
    ).toBeUndefined();
  });

  it("excludes only canonical voyage waits from sailing blockers", () => {
    const holds = [
      {
        ship_id: "ship-lapras",
        reason:
          "actual fleet confirms voyage in flight; waiting for authoritative arrival",
      },
      {
        ship_id: "ship-lapras",
        reason: "Insufficient provisions for departure",
      },
      {
        ship_id: "ship-lapras",
        reason: "passenger passage in flight; waiting for fare receipts",
      },
    ];
    expect(chartBlockingHolds(holds, "sailing")).toEqual([holds[1]]);
    expect(chartBlockingHolds(holds, "berthed")).toEqual(holds);
  });

  it.each([
    [
      "actual fleet confirms voyage in flight; waiting for authoritative arrival",
      "Waiting for arrival",
    ],
    ["Authoritative voyage still in flight", "Waiting for arrival"],
    ["waiting for authoritative arrival", "Waiting for arrival"],
    [
      "sourcing relocation waits for authoritative source arrival",
      "Repositioning to inspect a market",
    ],
    [
      "passenger passage in flight; waiting for fare receipts",
      "Carrying passengers to their destination",
    ],
  ])("shares the benign sailing wait vocabulary for %s", (reason, label) => {
    const wait = { ship_id: "ship-lapras", reason };
    expect(chartHoldLabel(reason)).toBe(label);
    expect(chartBlockingHolds([wait], "sailing")).toEqual([]);
    expect(chartBlockingHolds([wait], "berthed")).toEqual([wait]);
    const blocker = {
      ...wait,
      reason: `${reason}; destination and fleet disagree`,
    };
    expect(chartBlockingHolds([blocker], "sailing")).toEqual([blocker]);
  });

  it.each([
    [
      "no profitable executable voyage from calais; rejected alternative needs provisions and repair",
      "No profitable route found",
    ],
    [
      "no executable purchase-and-voyage alternative from calais",
      "No executable route found",
    ],
    [
      "berth-only operation waits for authoritative docking; already queued or unavailable",
      "Waiting for a berth",
    ],
    [
      "observe mode: durable mission retained without mutation",
      "Observation only; no actions are being sent",
    ],
    ["passenger loading waits for turnaround unlock", "Waiting for turnaround"],
    [
      "passenger repricing waits for twelve game hours at the current fare",
      "Waiting for passenger demand",
    ],
    [
      "passenger ladder retains each price for twelve game hours",
      "Waiting for passenger demand",
    ],
    [
      "passenger promise permits a bounded wait for one whole group",
      "Waiting for another passenger group",
    ],
  ])(
    "keeps routine activity neutral without hiding blockers: %s",
    (reason, label) => {
      const routine = { ship_id: "ship-lapras", reason };
      const blockers = [
        {
          ship_id: "ship-lapras",
          reason: "Insufficient provisions for departure",
        },
        {
          ship_id: "ship-lapras",
          reason: "Not enough cash to replenish stores",
        },
        { ship_id: "ship-lapras", reason: "Repairs required before departure" },
        { ship_id: "ship-lapras", reason: "Ship observation is stale" },
      ];
      expect(chartHoldLabel(reason)).toBe(label);
      for (const status of ["berthed", "anchored_roadstead", "sailing"]) {
        expect(chartBlockingHolds([routine], status)).toEqual([]);
        expect(chartBlockingHolds([routine, ...blockers], status)).toEqual(
          blockers,
        );
        expect(chartBlockingHolds([...blockers, routine], status)).toEqual(
          blockers,
        );
      }
    },
  );

  it.each([
    [
      "mission destination and actual fleet disagree; awaiting reconciliation",
      "Plan and ship records disagree",
    ],
    [
      "cannot replenish stores without spending protected regional bills reserve",
      "Funding needed",
    ],
    ["Insufficient provisions for departure", "Provisions needed"],
    ["Repairs required before departure", "Repairs needed"],
    ["constructor", "Action blocked"],
  ])("uses one concise current hold label for %s", (reason, label) => {
    expect(chartHoldLabel(reason)).toBe(label);
    const hold = { ship_id: "ship-lapras", reason };
    expect(chartBlockingHolds([hold], "sailing")).toEqual([hold]);
  });

  it("excludes account growth warnings and warnings for another ship", () => {
    const upkeep: EngineWarning = {
      code: "upkeep_seizure",
      severity: "critical",
      since: iso(NOW),
      reason: "Recorded seizure",
      names: { ship: "Lapras" },
      facts: {},
    };
    expect(
      chartShipWarnings(
        [
          upkeep,
          { ...upkeep, names: { ship: "Butterfree" } },
          {
            code: "falling_behind",
            severity: "warning",
            since: iso(NOW),
            reason: "Growth",
            names: {},
            facts: { ours_pence: 1, median_pence: 2, rival_count: 1 },
          },
        ],
        makeShip(),
      ),
    ).toEqual([upkeep]);
  });
});

describe("chart roster", () => {
  function vessel(
    name: string,
    arrival = "NaN",
    status = "sailing",
    used: number | null = 0,
    capacity: number | null = 100,
  ) {
    const detail = makeShip({ id: name, name, status }, []);
    detail.voyage = makeVoyage({ estimated_arrival_at: arrival });
    detail.cargo_used_tons = used;
    detail.effective_hold_capacity_tons = capacity;
    return {
      key: `a:${name}`,
      account: { label: "Yukine" },
      company: "Pokemon Pirates",
      detail,
      operation: {
        route: "London to Calais",
        activity: status,
        cargo: "Salt",
        cargoItems: undefined as ShipOperation["cargoItems"],
      },
    };
  }

  it("combines case-insensitive search across ship, company, account, route and cargo with state", () => {
    const lapras = vessel("Lapras", iso(ARRIVE), "sailing");
    const bulbasaur = vessel("Bulbasaur", "NaN", "berthed");
    for (const term of [
      " lapRAS ",
      "POKEMON",
      "Yukine",
      "CALais",
      "salt",
      "SAILING",
    ]) {
      expect(chartRoster([bulbasaur, lapras], term, "sailing", "name")).toEqual(
        [lapras],
      );
    }
    expect(chartRoster([lapras], "timber", "", "name")).toEqual([]);
  });

  it("searches every named cargo item beyond the compact two-goods summary", () => {
    const row = vessel("Lapras");
    row.detail.cargo_manifest = { grain: 40, salt: 30, timber: 20, wool: 10 };
    const catalog: Catalog = {
      ports: [],
      ship_classes: [],
      sea_zones: [],
      commodities: [
        ["grain", "Milled grain"],
        ["salt", "Rock salt"],
        ["timber", "Oak timber"],
        ["wool", "Merino wool"],
      ].map(([id, name]) => ({
        id,
        name,
        category: "goods",
        base_price_pence: 100,
      })),
    };
    const account = makeAccount([row.detail]);
    account.snapshot.catalog = catalog;
    row.operation = shipOperation(account, row.detail, NOW);
    expect(row.operation.cargo).toBe(
      "Cargo 40 t Milled grain, 30 t Rock salt +2 goods",
    );
    for (const term of [
      "MILLED GRAIN",
      "rock salt",
      "oak TIMBER",
      " merino wool ",
    ]) {
      expect(chartRoster([row], term, "sailing", "name", catalog)).toEqual([
        row,
      ]);
    }
    expect(chartRoster([row], "cotton", "", "name", catalog)).toEqual([]);
    expect(
      chartRoster([row], "merino wool", "berthed", "name", catalog),
    ).toEqual([]);
    const unknown = {
      ...row,
      operation: {
        ...row.operation,
        cargoItems: undefined,
        cargo: "Cargo not recorded",
      },
    };
    expect(chartRoster([unknown], "merino wool", "", "name", catalog)).toEqual(
      [],
    );
  });

  it("retains omitted loss rows and filters and searches their displayed fates", () => {
    const fates = [
      ["lost_at_sea", "Sunk"],
      ["captured", "Captured"],
      ["missing", "Missing from fleet list"],
      ["broken_up", "Broken up"],
      ["royal_navy", "Commissioned to the Royal Navy"],
      ["seized_smuggling", "Seized for smuggling"],
    ];
    const account = makeAccount(
      [makeShip()],
      fates.map(([fate]) =>
        makeLoss({
          ship_id: fate,
          ship_name: fate,
          fate,
          last_port_id: null,
          voyage_origin_id: null,
        }),
      ),
    );
    account.ship_losses?.push(
      makeLoss({ fate: "missing", recovered_at: iso(NOW) }),
    );
    const rows = fleet(account).map((detail) => ({
      key: `${account.id}:${detail.ship.id}`,
      account,
      company: "Pokemon Pirates",
      detail,
      operation: shipOperation(account, detail, NOW),
    }));
    expect(
      rows.filter((row) => row.detail.ship.status === "sunk"),
    ).toHaveLength(fates.length);
    expect(chartRoster(rows, "", "sunk", "name")).toHaveLength(fates.length);
    expect(
      chartRoster(rows, "LOSS", "", "name").map((row) => row.detail.ship.id),
    ).toEqual(["captured", "lost_at_sea"]);
    for (const [fate, label] of fates) {
      const row = rows.find((entry) => entry.detail.ship.id === fate);
      expect(row?.operation.activity).toBe(label);
      expect(chartRoster(rows, label.toUpperCase(), "sunk", "name")).toEqual([
        row,
      ]);
      expect(chartRoster(rows, fate, "sunk", "name")).toEqual([row]);
      expect(chartRoster(rows, "", label, "name")).toEqual([row]);
    }
    expect(
      chartRoster(rows, "", "Sailing", "name").map((row) => row.detail.ship.id),
    ).toEqual(["ship-lapras"]);
    expect(chartRoster(rows, "missing", "sailing", "name")).toEqual([]);
    expect(
      chartRoster(rows, "", "", "state").map((row) => row.operation.activity),
    ).toEqual([
      "Broken up",
      "Captured",
      "Commissioned to the Royal Navy",
      "Missing from fleet list",
      "Sailing",
      "Seized for smuggling",
      "Sunk",
    ]);
  });

  it("searches displayed loss ports by ID, humanized ID and accented catalogue aliases", () => {
    const catalog: Catalog = {
      ports: [
        ["konigsberg", "Königsberg"],
        ["st_peter_port", "Saint Peter Port"],
      ].map(([id, name]) => ({
        id,
        name,
        banking_region: "europe",
        sea_zone_id: "",
        phase: 1,
        is_hub: false,
        has_shipyard: false,
        customs_bps: 0,
        base_berths: 1,
        effective_berths: 1,
        total_warehouse_cap: 0,
        produces: [],
        manufactures: [],
        imports: [],
      })),
      ship_classes: [],
      commodities: [],
      sea_zones: [],
    };
    const account = makeAccount(
      [
        makeShip({
          id: "legacy",
          status: "sunk",
          current_port_id: "st_peter_port",
        }),
        makeShip({
          id: "fallback",
          status: "sunk",
          current_port_id: "london",
        }),
        makeShip({
          id: "relost",
          status: "sunk",
          current_port_id: "calais",
        }),
        makeShip({
          id: "unknown",
          status: "sunk",
          current_port_id: "uncharted_port",
        }),
      ],
      [
        makeLoss({
          ship_id: "observed",
          last_port_id: "konigsberg",
          last_sea_zone_id: null,
        }),
        makeLoss({
          ship_id: "origin",
          last_port_id: null,
          voyage_origin_id: "st_peter_port",
        }),
        makeLoss({
          ship_id: "fallback",
          last_port_id: null,
          voyage_origin_id: null,
        }),
        makeLoss({
          ship_id: "relost",
          last_port_id: "konigsberg",
          last_sea_zone_id: null,
          recovered_at: iso(NOW),
        }),
      ],
    );
    const rows = fleet(account).map((detail) => ({
      key: `${account.id}:${detail.ship.id}`,
      account,
      company: "Pokemon Pirates",
      detail,
      operation: shipOperation(account, detail, NOW),
    }));
    const cases: [string, string[]][] = [
      ["konigsberg", ["observed"]],
      ["KÖNIGSBERG", ["observed"]],
      ["st_peter_port", ["legacy"]],
      ["St peter port", ["legacy"]],
      ["SAINT PETER PORT", ["legacy"]],
      ["london", []],
      ["calais", ["relost"]],
      ["uncharted_port", ["unknown"]],
      ["Uncharted port", ["unknown"]],
    ];
    for (const [term, expected] of cases) {
      expect(
        chartRoster(rows, term, "sunk", "name", catalog).map(
          (row) => row.detail.ship.id,
        ),
      ).toEqual(expected);
    }
    expect(chartRoster(rows, "Königsberg", "sunk", "name")).toEqual([]);
  });

  it("sorts finite arrivals first and treats Infinity, NaN and unavailable arrivals as unknown", () => {
    const input = [
      vessel("Zeta", "Infinity"),
      vessel("Later", iso(ARRIVE + 1000)),
      vessel("Alpha", "NaN"),
      vessel("Earlier", iso(ARRIVE)),
      vessel("Berthed", iso(NOW), "berthed"),
    ];
    expect(
      chartRoster(input, "", "", "arrival").map((row) => row.detail.ship.name),
    ).toEqual(["Earlier", "Later", "Alpha", "Berthed", "Zeta"]);
    expect(input[0].detail.ship.name).toBe("Zeta");
  });

  it("sorts states and breaks equal names by account-qualified identity", () => {
    const sailing = vessel("Zeta", "", "sailing");
    const berthed = vessel("Alpha", "", "berthed");
    const anchored = vessel("Beta", "", "anchored_roadstead");
    expect(chartRoster([sailing, berthed, anchored], "", "", "state")).toEqual([
      anchored,
      berthed,
      sailing,
    ]);
    const first = { ...berthed, key: "a:Alpha" };
    const second = { ...berthed, key: "b:Alpha" };
    expect(chartRoster([second, first], "", "", "name")).toEqual([
      first,
      second,
    ]);
  });

  it("sorts cargo by fill ratio, including recorded empty holds, with invalid capacity last", () => {
    const input = [
      vessel("Unknown", "", "berthed", 5, 0),
      vessel("Half", "", "berthed", 5, 10),
      vessel("Empty", "", "berthed", 0, 10),
      vessel("Full", "", "berthed", 20, 20),
      vessel("Infinite", "", "berthed", 5, Infinity),
      vessel("Malformed", "", "berthed", NaN, 10),
      vessel("Unknown capacity", "", "sunk", 0, null),
      vessel("Unknown cargo", "", "sunk", null, 100),
    ];
    expect(
      chartRoster(input, "", "", "cargo").map((row) => row.detail.ship.name),
    ).toEqual([
      "Full",
      "Half",
      "Empty",
      "Infinite",
      "Malformed",
      "Unknown",
      "Unknown capacity",
      "Unknown cargo",
    ]);
  });
});

describe("chart deep links and URL selection", () => {
  const account = (id: string, shipID: string): Account => ({
    id,
    label: id,
    company_id: id,
    enabled: true,
    created_at: iso(NOW),
    snapshot: { ships: [makeShip({ id: shipID })] },
  });

  it("resolves account-qualified ships and rejects wrong, missing and ambiguous identities", () => {
    const accounts = [
      account("one", "shared"),
      account("two", "shared"),
      account("three", "unique"),
    ];
    expect(resolveChartDeepLink(accounts, "?ship=shared&account=two")).toEqual({
      shipID: "shared",
      accountID: "two",
    });
    expect(resolveChartDeepLink(accounts, "?ship=unique")).toEqual({
      shipID: "unique",
      accountID: "three",
    });
    for (const query of [
      "?ship=shared",
      "?ship=missing",
      "?ship=unique&account=one",
      "?account=one",
      "",
    ]) {
      expect(resolveChartDeepLink(accounts, query)).toBeUndefined();
    }
  });

  it("synchronizes both IDs while preserving unrelated query values and the fragment", () => {
    const url = chartSelectionURL(
      "https://dashboard.example/chart?layer=ports&ship=old&account=old#key",
      {
        shipID: "ship & one",
        accountID: "account/two",
      },
    );
    expect(new URL(url).searchParams.get("ship")).toBe("ship & one");
    expect(new URL(url).searchParams.get("account")).toBe("account/two");
    const cleared = new URL(chartSelectionURL(url));
    expect(cleared.search).toBe("?layer=ports");
    expect(cleared.hash).toBe("#key");
    expect(cleared.pathname).toBe("/chart");
  });

  it("clears an invalid deep link without retaining a mismatched account parameter", () => {
    const href =
      "https://dashboard.example/chart?ship=missing&account=wrong&routes=off";
    const selection = resolveChartDeepLink(
      [account("one", "ship")],
      new URL(href).search,
    );
    expect(chartSelectionURL(href, selection)).toBe(
      "https://dashboard.example/chart?routes=off",
    );
  });
});

describe("ship warning assessment availability", () => {
  const report: WarningReport = {
    generated_at: iso(NOW),
    build: { revision: null, modified: false, started_at: iso(NOW) },
    accounts: [
      {
        account_id: "a",
        label: "A",
        available: true,
        error_code: "",
        suppressed: [],
        warnings: [],
      },
    ],
    unmonitored: [],
  };

  it("distinguishes pending and failed queries from an assessed empty result", () => {
    expect(chartWarningAssessment(undefined, "a", "pending", true).status).toBe(
      "unassessed",
    );
    expect(chartWarningAssessment(undefined, "a", "error", true).status).toBe(
      "unavailable",
    );
    expect(chartWarningAssessment(report, "a", "error", true)).toMatchObject({
      status: "unavailable",
      message: expect.stringContaining("retained"),
    });
    expect(chartWarningAssessment(report, "a", "success", true)).toEqual({
      status: "assessed",
    });
  });

  it("exposes account evaluation errors, missing assessments and suppressed ship warning classes", () => {
    expect(
      chartWarningAssessment(
        {
          ...report,
          accounts: [
            { ...report.accounts[0], error_code: "snapshot_unavailable" },
          ],
        },
        "a",
        "success",
        true,
      ).status,
    ).toBe("unavailable");
    expect(
      chartWarningAssessment(report, "missing", "success", true).status,
    ).toBe("unavailable");
    for (const code of ["hold_stuck", "upkeep_seizure"] as const) {
      expect(
        chartWarningAssessment(
          {
            ...report,
            accounts: [{ ...report.accounts[0], suppressed: [code] }],
          },
          "a",
          "success",
          true,
        ).status,
      ).toBe("unassessed");
    }
    expect(
      chartWarningAssessment(
        {
          ...report,
          accounts: [{ ...report.accounts[0], suppressed: ["falling_behind"] }],
        },
        "a",
        "success",
        true,
      ).status,
    ).toBe("assessed");
    expect(chartWarningAssessment(report, "a", "success", false).status).toBe(
      "unassessed",
    );
  });
});
