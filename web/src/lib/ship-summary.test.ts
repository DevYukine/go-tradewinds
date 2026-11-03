import { describe, expect, it } from "vitest";
import { chartHoldLabel } from "./chart";
import { shipCargo, shipOperation } from "./operations";
import { shipPassengers, shipSummary, shortShipReason } from "./ship-summary";
import type {
  Account,
  Catalog,
  Decision,
  PassengerAboardGroup,
  ShipDetail,
  ShipLoss,
} from "./types";

const observed = "2026-11-03T12:00:00Z";
const now = Date.parse(observed) + 5_000;
const meta = { generation: "capture", complete: true, updated_at: observed };
const detail: ShipDetail = {
  ship: {
    id: "ship",
    company_id: "company",
    name: "Butterfree",
    ship_class: "barque",
    status: "sailing",
    passenger_hold_tons: 0,
    current_leg_idx: 0,
    cannons_count: 2,
    stores_provisions_units: 1_000_000,
    updated_at: observed,
  },
  hold_capacity_tons: 220,
  effective_hold_capacity_tons: 208,
  cargo_used_tons: 88,
  cargo_manifest: { provisions: 88 },
  stores: { provisions_tons: 1, sea_days_remaining: 8 },
  route_legs: [
    {
      from_node: "amsterdam",
      to_node: "cape_verde",
      distance_nm: 2000,
      edge_class: "ocean",
      is_sound_toll: false,
    },
  ],
};
const portFacts = {
  banking_region: "europe",
  sea_zone_id: "channel",
  phase: 1,
  is_hub: false,
  has_shipyard: false,
  customs_bps: 0,
  base_berths: 10,
  effective_berths: 10,
  total_warehouse_cap: 100,
  produces: [],
  manufactures: [],
  imports: [],
};
const catalog: Catalog = {
  ports: [
    { ...portFacts, id: "amsterdam", name: "Amsterdam" },
    { ...portFacts, id: "cape_verde", name: "Santiago (Cape Verde)" },
    { ...portFacts, id: "calais", name: "Calais" },
  ],
  ship_classes: [
    {
      id: "barque",
      name: "Barque",
      crew_required: 40,
      hull_category: "ocean",
      phase: 2,
      base_speed_centiknots: 400,
      hold_capacity_tons: 220,
      max_cannons: 12,
      base_labor_cost_pence: 1000,
      build_materials: [],
    },
  ],
  commodities: [
    {
      id: "provisions",
      name: "Provisions",
      category: "stores",
      base_price_pence: 100,
    },
  ],
  sea_zones: [],
};
function account(
  reason = "actual fleet confirms voyage in flight; waiting for authoritative arrival",
): Account {
  return {
    id: "account",
    label: "Yukine",
    company_id: "company",
    enabled: true,
    created_at: observed,
    snapshot: {
      ships: [detail],
      catalog,
      _engine: {
        generation: "capture",
        observed_at: observed,
        assessed_ship_ids: ["ship"],
        holds: [{ ship_id: "ship", reason }],
      },
      _meta: { ships: meta, _engine: meta },
    },
  };
}
const mission = {
  ship_id: "ship",
  phase: "sailing",
  updated_at: observed,
  plan: {
    kind: "charter",
    origin: "amsterdam",
    destination: "cape_verde",
    commodity: "provisions",
    quantity: 88,
    economics: { expected_net_pence: 1200, basis: "destination" },
  },
};
const decision: Decision = {
  id: 1,
  account_id: "account",
  ship_id: "ship",
  kind: "rejected:spot",
  reason:
    "non-positive stressed destination contribution after fitted receipt uncertainty",
  created_at: observed,
  details: {
    accepted: false,
    destination: "calais",
    commodity: "grain",
    quantity: 10,
    economics: {},
  },
};
const group: PassengerAboardGroup = {
  id: "group",
  kind: "travellers",
  size: 7,
  origin_port_id: "amsterdam",
  dest_port_id: "cape_verde",
  fare_per_head_pence: 100,
  full_fare_pence: 700,
  hold_tons: 7,
};
const loss: ShipLoss = {
  account_id: "account",
  ship_id: "ship",
  ship_name: "Butterfree",
  ship_class: "barque",
  status: "sunk",
  fate: "lost_at_sea",
  observed_at: observed,
  last_observed_at: observed,
  recovered_at: null,
  last_port_id: null,
  last_sea_zone_id: null,
  voyage_origin_id: "amsterdam",
  voyage_destination_id: "cape_verde",
  cargo_manifest: { provisions: 88 },
  stores: { provisions_tons: 1, sea_days_remaining: 8 },
  hazard_cause: "storm",
  insurance_claim_paid_pence: null,
};

describe("compact ship summary", () => {
  it("prefers the durable sailing mission over an ordinary arrival wait or rejected alternative", () => {
    const result = shipSummary(
      { ...account(), missions: [mission] },
      detail,
      [decision],
      now,
    );
    expect(result.action).toBe("Sailing to Santiago (Cape Verde)");
    expect(result.cargo).toBe("Cargo 88 t Provisions");
    expect(result.crew).toBe(40);
    expect(result.passengers.text).toBe("Passengers 0");
    expect(result.why).toEqual({
      label: "Why",
      text: "Charter 88 t Provisions to Santiago (Cape Verde).",
      attention: false,
    });
  });
  it.each([30_000, 119_999])(
    "keeps delayed observations useful without attention at %i ms",
    (age) => {
      const delayedNow = Date.parse(observed) + age;
      const result = shipSummary(
        { ...account(), missions: [mission] },
        detail,
        [decision],
        delayedNow,
      );
      expect(result.operation.observation.state).toBe("delayed");
      expect(result.operation.holdsCurrent).toBe(true);
      expect(result.action).toBe("Sailing to Santiago (Cape Verde)");
      expect(result.why).toEqual({
        label: "Why",
        text: "Charter 88 t Provisions to Santiago (Cape Verde).",
        attention: false,
      });
      expect(shipSummary(account(), detail, [], delayedNow).why).toEqual({
        label: "Why",
        text: "Waiting for arrival.",
        attention: false,
      });
      const blocker = shipSummary(
        {
          ...account(
            "mission destination and actual fleet disagree; waiting for reconciliation",
          ),
          missions: [mission],
        },
        detail,
        [],
        delayedNow,
      );
      expect(blocker.why).toEqual({
        label: "Why",
        text: "Plan and ship records disagree.",
        attention: true,
      });
    },
  );
  it.each([
    "mission destination and actual fleet disagree; waiting for reconciliation",
    "cannot replenish stores without spending protected regional bills reserve",
  ])("shows the current blocker instead of a durable mission: %s", (reason) => {
    const result = shipSummary(
      { ...account(reason), missions: [mission] },
      detail,
      [decision],
      now,
    );
    expect(result.why).toEqual({
      label: "Why",
      text: `${chartHoldLabel(reason)}.`,
      attention: true,
    });
    expect(result.plan?.source).toBe("mission");
  });
  it.each([false, true])(
    "puts blockers before a benign wait and counts same-label blockers (mission=%s)",
    (withMission) => {
      const value = account();
      const engine = value.snapshot._engine as Record<string, unknown>;
      engine.holds = [
        {
          ship_id: "ship",
          reason:
            "actual fleet confirms voyage in flight; waiting for authoritative arrival",
        },
        { ship_id: "ship", reason: "Not enough cash to replenish stores" },
        { ship_id: "ship", reason: "Protected regional balance is committed" },
        { ship_id: "ship", reason: "Repairs required before departure" },
      ];
      const result = shipSummary(
        { ...value, missions: withMission ? [mission] : [] },
        detail,
        [],
        now,
      );
      expect(result.why).toEqual({
        label: "Why",
        text: "Funding needed. +2 other blockers",
        attention: true,
      });
    },
  );
  it.each([
    [
      "passenger loading waits for turnaround unlock",
      "Waiting for turnaround.",
    ],
    [
      "passenger repricing waits for twelve game hours at the current fare",
      "Waiting for passenger demand.",
    ],
    [
      "passenger ladder retains each price for twelve game hours",
      "Waiting for passenger demand.",
    ],
    [
      "passenger promise permits a bounded wait for one whole group",
      "Waiting for another passenger group.",
    ],
    [
      "berth-only operation waits for authoritative docking; already queued or unavailable",
      "Waiting for a berth.",
    ],
    [
      "observe mode: durable mission retained without mutation",
      "Observation only; no actions are being sent.",
    ],
    [
      "no profitable executable voyage from calais; rejected alternative needs provisions and repair",
      "No profitable route found.",
    ],
  ])(
    "keeps a port wait neutral and lets a real blocker take priority: %s",
    (reason, text) => {
      for (const status of ["berthed", "anchored_roadstead"]) {
        const atPort = {
          ...detail,
          ship: { ...detail.ship, status, current_port_id: "calais" },
          route_legs: [],
        };
        const value = account(reason);
        expect(shipSummary(value, atPort, [], now).why).toEqual({
          label: "Why",
          text,
          attention: false,
        });
        expect(
          shipSummary(
            { ...value, missions: [{ ...mission, phase: "loading" }] },
            atPort,
            [],
            now,
          ).why,
        ).toEqual({
          label: "Why",
          text: "Charter 88 t Provisions to Santiago (Cape Verde).",
          attention: false,
        });
        const engine = value.snapshot._engine as Record<string, unknown>;
        engine.holds = [
          { ship_id: "ship", reason },
          { ship_id: "ship", reason: "Not enough cash to replenish stores" },
          { ship_id: "ship", reason: "Repairs required before departure" },
        ];
        expect(
          shipSummary({ ...value, missions: [mission] }, atPort, [], now).why,
        ).toEqual({
          label: "Why",
          text: "Funding needed. +1 other blocker",
          attention: true,
        });
      }
    },
  );
  it.each([
    ["spot", "Trade"],
    ["charter", "Charter"],
    ["spot+freight", "Trade and freight"],
    ["charter+freight", "Charter and freight"],
    ["recovery", "Voyage recovery"],
  ])(
    "names catalog cargo and the %s mission without an inner colon",
    (kind, name) => {
      const value = account();
      value.snapshot.catalog = {
        ...catalog,
        commodities: [
          {
            id: "canvas_sails",
            name: "Sailcloth and canvas",
            category: "manufactured",
            base_price_pence: 100,
          },
        ],
      };
      const result = shipSummary(
        {
          ...value,
          missions: [
            {
              ...mission,
              plan: { ...mission.plan, kind, commodity: "canvas_sails" },
            },
          ],
        },
        detail,
        [],
        now,
      );
      expect(result.why.text).toBe(
        `${name} 88 t Sailcloth and canvas to Santiago (Cape Verde).`,
      );
      expect(`Why: ${result.why.text}`.match(/:/g)).toHaveLength(1);
      expect(result.why.attention).toBe(false);
    },
  );
  it("names recovery without inventing a cargo load", () => {
    expect(
      shipSummary(
        {
          ...account(),
          missions: [
            {
              ...mission,
              plan: {
                ...mission.plan,
                kind: "recovery",
                commodity: "",
                quantity: 0,
              },
            },
          ],
        },
        detail,
        [],
        now,
      ).why.text,
    ).toBe("Voyage recovery to Santiago (Cape Verde).");
  });
  it("describes an aborted durable plan as closing rather than a new destination", () => {
    const result = shipSummary(
      {
        ...account(),
        missions: [
          {
            ...mission,
            phase: "closing",
            plan: { ...mission.plan, kind: "aborted", quantity: 0 },
          },
        ],
      },
      detail,
      [decision],
      now,
    );
    expect(result.why.text).toBe("Closing the previous plan.");
    expect(result.why.attention).toBe(false);
  });
  it.each(["return", "return_depart", "return_sailing"])(
    "uses the return destination without repeating delivered cargo in phase %s",
    (phase) => {
      const returning = {
        ...detail,
        cargo_manifest: {},
        cargo_used_tons: 0,
        route_legs: [
          {
            ...detail.route_legs![0],
            from_node: "cape_verde",
            to_node: "amsterdam",
          },
        ],
      };
      const result = shipSummary(
        { ...account(), missions: [{ ...mission, phase }] },
        returning,
        [],
        now,
      );
      expect(result.action).toBe("Sailing to Amsterdam");
      expect(result.cargo).toBe("No trade cargo recorded");
      expect(result.why.text).toBe("Returning to Amsterdam.");
      expect(result.plan?.phase).toBe(phase);
    },
  );
  it.each([
    ["closing", "Closing the previous plan."],
    ["settling", "Reconciling the completed voyage."],
  ])(
    "does not describe delivery as future work in phase %s",
    (phase, reason) => {
      expect(
        shipSummary(
          { ...account(), missions: [{ ...mission, phase }] },
          { ...detail, cargo_manifest: {}, cargo_used_tons: 0 },
          [],
          now,
        ).why.text,
      ).toBe(reason);
    },
  );
  it("authors maintenance and missing destination plans without awkward fallback copy", () => {
    expect(
      shipSummary(
        {
          ...account(),
          missions: [
            {
              ...mission,
              phase: "maintenance",
              plan: {
                ...mission.plan,
                kind: "maintenance",
                destination: "",
                quantity: 0,
              },
            },
          ],
        },
        detail,
        [],
        now,
      ).why.text,
    ).toBe("Maintenance in progress.");
    expect(
      shipSummary(
        {
          ...account(),
          missions: [
            { ...mission, plan: { ...mission.plan, destination: "" } },
          ],
        },
        detail,
        [],
        now,
      ).why.text,
    ).toBe("Charter 88 t Provisions; destination not recorded.");
  });
  it("keeps an anchored ship without route legs anchored and its ordinary idle reason neutral", () => {
    const anchored = {
      ...detail,
      ship: {
        ...detail.ship,
        status: "anchored_roadstead",
        current_port_id: "calais",
      },
      route_legs: [],
    };
    const result = shipSummary(
      account(
        "no profitable executable voyage from calais; rejected alternative needs provisions and repair",
      ),
      anchored,
      [],
      now,
    );
    expect(result.action).toBe("Anchored off Calais");
    expect(result.why.text).toBe("No profitable route found.");
    expect(result.why.attention).toBe(false);
  });
  it("uses genuinely berthed wording only for a berthed status", () => {
    expect(
      shipSummary(
        account(),
        {
          ...detail,
          ship: {
            ...detail.ship,
            status: "berthed",
            current_port_id: "calais",
          },
          route_legs: [],
        },
        [],
        now,
      ).action,
    ).toBe("Berthed in Calais");
  });
  it("does not turn normal in-flight waiting into a problem", () => {
    expect(shipSummary(account(), detail, [], now).why).toEqual({
      label: "Why",
      text: "Waiting for arrival.",
      attention: false,
    });
  });
  it("shows stale state and starvation together instead of covering them with a mission", () => {
    const staleNow = now + 180_000;
    const starving = {
      ...detail,
      voyage: {
        current_leg_idx: 0,
        speed_centiknots: 400,
        estimated_arrival_at: observed,
        legs: [
          {
            leg_idx: 0,
            from_node: "amsterdam",
            to_node: "cape_verde",
            distance_nm: 2000,
            speed_centiknots: 400,
            estimated_arrival_at: observed,
            starving: true,
          },
        ],
      },
    };
    const result = shipSummary(
      { ...account(), missions: [mission] },
      starving,
      [],
      staleNow,
    );
    expect(result.action).toMatch(/^Last recorded: Sailing/);
    expect(result.why.attention).toBe(true);
    expect(result.why.text).toContain("Stores run out");
    expect(result.why.text).toContain("Ship records are stale.");
    expect(result.eta).toBe("Awaiting updated arrival");
  });
  it.each([
    ["lost_at_sea", "Sunk"],
    ["captured", "Captured"],
    ["seized_smuggling", "Seized for smuggling"],
  ])(
    "retains %s fate without reviving the mission or current passengers",
    (fate, label) => {
      const result = shipSummary(
        { ...account(), missions: [mission] },
        { ...detail, ship: { ...detail.ship, status: "sunk", fate } },
        [],
        now,
      );
      expect(result.action).toBe(label);
      expect(result.cargo).toBe("Last afloat cargo not recorded");
      expect(result.why.text).toBe("");
      expect(result.plan).toBeUndefined();
      expect(result.operation.starving).toBe(false);
    },
  );
  it.each([
    ["storm", "Storm."],
    ["pirate_attack", "Pirate attack."],
    ["storm.", "Storm."],
    [null, ""],
    [" ", ""],
  ])(
    "uses the recorded hazard cause without repeating the fate (%s)",
    (cause, text) => {
      const result = shipSummary(
        { ...account(), missions: [mission] },
        {
          ...detail,
          ship: { ...detail.ship, status: "sunk", fate: "lost_at_sea" },
          loss: { ...loss, hazard_cause: cause },
        },
        [decision],
        now,
      );
      expect(result.why).toEqual({
        label: "Why",
        text,
        attention: true,
      });
      expect(result.action).toBe("Sunk");
      expect(result.plan).toBeUndefined();
    },
  );
  it("uses historical labels when the current hold generations disagree", () => {
    const value = account("no profitable executable voyage from calais");
    (value.snapshot._meta as Record<string, unknown>).ships = {
      ...meta,
      generation: "newer",
    };
    const result = shipSummary(
      value,
      detail,
      [
        decision,
        { ...decision, id: 2, created_at: "invalid", reason: "Wrong latest" },
      ],
      now,
    );
    expect(result.why).toEqual({
      label: "Last decision",
      text: "Route rejected; open details.",
      attention: false,
    });
    expect(result.action).not.toContain("Calais");
  });
  it("does not replace a failed mission projection with a current-looking historical plan", () => {
    const result = shipSummary(
      { ...account(), missions_unavailable: true, missions: [mission] },
      detail,
      [decision],
      now,
    );
    expect(result.why.text).toBe("Waiting for arrival.");
    expect(result.plan).toBeUndefined();
  });
  it("keeps absent reasons honest instead of reporting a healthy hold assessment", () => {
    const value = account();
    value.snapshot._engine = {
      generation: "capture",
      observed_at: observed,
      assessed_ship_ids: ["ship"],
      holds: [],
    };
    expect(shipSummary(value, detail, [], now).why.text).toBe(
      "Reason not recorded.",
    );
  });
  it.each([
    ["spot", true, 5000, "Trade modeled profitable."],
    ["charter", true, 5000, "Trade modeled profitable."],
    ["freight", true, 5000, "Trade modeled profitable."],
    ["spot+freight", true, 5000, "Trade modeled profitable."],
    ["charter+freight", true, 5000, "Trade modeled profitable."],
    ["rejected:spot", true, 5000, "Route rejected; open details."],
    ["spot", false, -5000, "Route rejected; open details."],
    ["spot", true, -5000, "Reason recorded; open details."],
    ["spot", true, 0, "Reason recorded; open details."],
    ["scout", true, -5000, "Reason recorded; open details."],
    ["relocation", true, -5000, "Reason recorded; open details."],
    ["scout", true, 5000, "Reason recorded; open details."],
    ["relocation", true, 5000, "Reason recorded; open details."],
    ["recovery", true, 5000, "Reason recorded; open details."],
  ])(
    "keeps a priced %s decision's acceptance distinct from profitability (accepted=%s, net=%i)",
    (kind, accepted, net, expected) => {
      const value = account();
      value.snapshot._engine = {
        generation: "capture",
        observed_at: observed,
        assessed_ship_ids: ["ship"],
        holds: [],
      };
      expect(
        shipSummary(
          value,
          detail,
          [
            {
              ...decision,
              kind,
              reason: "below 2% minimum margin on purchase plus customs",
              details: { accepted, economics: { expected_net_pence: net } },
            },
          ],
          now,
        ).why.text,
      ).toBe(expected);
    },
  );
  it.each(["scout", "relocation"])(
    "does not turn an accepted %s's expected cost into a rejection",
    (kind) => {
      const value = account();
      (value.snapshot._engine as Record<string, unknown>).holds = [];
      const result = shipSummary(
        value,
        detail,
        [
          {
            ...decision,
            kind,
            details: {
              accepted: true,
              economics: { expected_net_pence: -5000 },
            },
          },
        ],
        now,
      );
      expect(result.why).toEqual({
        label: "Last decision",
        text: "Reason recorded; open details.",
        attention: false,
      });
      expect(result.decision?.details).toMatchObject({ accepted: true });
    },
  );
  it.each([null, undefined])(
    "keeps null or absent decision details safe (%j)",
    (details) => {
      const value = account();
      value.snapshot._engine = {
        generation: "capture",
        observed_at: observed,
        assessed_ship_ids: ["ship"],
        holds: [],
      };
      expect(
        shipSummary(
          value,
          detail,
          [
            {
              ...decision,
              kind: "hold",
              reason: "An unfamiliar hold without a complete sentence",
              details,
            },
          ],
          now,
        ).why.text,
      ).toBe("Reason recorded; open details.");
      expect(shortShipReason("  ")).toBe("Reason not recorded.");
    },
  );
  it.each(["broken_up", "royal_navy", "missing", "unmapped_fate"])(
    "does not turn a non-loss fate into a red loss warning (%s)",
    (fate) => {
      expect(
        shipSummary(
          account(),
          { ...detail, ship: { ...detail.ship, status: "sunk", fate } },
          [],
          now,
        ).why.attention,
      ).toBe(false);
    },
  );
  it("keeps singular cargo kinds and nonzero short stores readable", () => {
    expect(
      shipSummary(
        account(),
        { ...detail, cargo_manifest: { grain: 5, timber: 4, wool: 3 } },
        [],
        now,
      ).cargo,
    ).toContain("+1 good");
    expect(
      shipSummary(
        account(),
        { ...detail, stores: { provisions_tons: 1, sea_days_remaining: 1 } },
        [],
        now,
      ).stores,
    ).toBe("Stores 1 sea day");
    for (const seaDays of [0.99, 1.01]) {
      expect(
        shipSummary(
          account(),
          {
            ...detail,
            stores: { provisions_tons: 1, sea_days_remaining: seaDays },
          },
          [],
          now,
        ).stores,
      ).toBe("Stores 1 sea day");
    }
    expect(
      shipSummary(
        account(),
        { ...detail, stores: { provisions_tons: 1, sea_days_remaining: 0.01 } },
        [],
        now,
      ).stores,
    ).toBe("Stores <0.1 sea days");
  });
  it("shows two largest cargo stacks plus the remaining kind count, not a long manifest", () => {
    const result = shipSummary(
      account(),
      {
        ...detail,
        cargo_manifest: {
          grain: 20,
          provisions: 50,
          timber: 10,
          wool: 5,
          empty: 0,
        },
      },
      [],
      now,
    );
    expect(result.cargo).toBe("Cargo 50 t Provisions, 20 t Grain +2 goods");
    expect(result.cargoItems).toEqual([
      { id: "provisions", name: "Provisions", tons: 50 },
      { id: "grain", name: "Grain", tons: 20 },
      { id: "timber", name: "Timber", tons: 10 },
      { id: "wool", name: "Wool", tons: 5 },
    ]);
    expect(result.cargo).toBe(result.operation.cargo);
    expect(result.cargoItems).toBe(result.operation.cargoItems);
    expect(
      shipSummary(account(), { ...detail, cargo_manifest: {} }, [], now).cargo,
    ).toBe("No trade cargo recorded");
    expect(
      shipSummary(account(), { ...detail, cargo_manifest: null }, [], now)
        .cargo,
    ).toBe("Cargo not recorded");
  });
  it("agrees with shipOperation and shared cargo for multiple, empty and unknown loads", () => {
    const cases: {
      manifest: ShipDetail["cargo_manifest"];
      expected: string;
    }[] = [
      {
        manifest: { provisions: 50, grain: 20, timber: 10, wool: 5 },
        expected: "Cargo 50 t Provisions, 20 t Grain +2 goods",
      },
      { manifest: {}, expected: "No trade cargo recorded" },
      { manifest: { provisions: 0 }, expected: "No trade cargo recorded" },
      { manifest: null, expected: "Cargo not recorded" },
    ];
    for (const { manifest, expected } of cases) {
      const value = account();
      const ship = { ...detail, cargo_manifest: manifest };
      const result = shipSummary(value, ship, [], now);
      const operation = shipOperation(value, ship, now);
      const cargo = shipCargo(ship, catalog);
      expect(result.cargo).toBe(expected);
      expect(result.cargo).toBe(operation.cargo);
      expect(result.cargo).toBe(cargo.text);
      expect(result.cargoItems).toEqual(operation.cargoItems);
      expect(result.cargoItems).toEqual(cargo.items);
      expect(result.cargoItems === undefined).toBe(manifest === null);
    }
  });
  it("uses the fallback catalog consistently for mission, cargo and operation", () => {
    const value = account();
    delete value.snapshot.catalog;
    const namedCatalog = {
      ...catalog,
      commodities: [
        {
          id: "provisions",
          name: "Ship stores",
          category: "stores",
          base_price_pence: 100,
        },
      ],
    };
    const result = shipSummary(
      { ...value, missions: [mission] },
      detail,
      [],
      now,
      namedCatalog,
    );
    expect(result.cargo).toBe("Cargo 88 t Ship stores");
    expect(result.cargo).toBe(result.operation.cargo);
    expect(result.cargoItems).toEqual([
      { id: "provisions", name: "Ship stores", tons: 88 },
    ]);
    expect(result.why.text).toBe(
      "Charter 88 t Ship stores to Santiago (Cape Verde).",
    );
  });
  it("does not derive a new cause from a rejected alternative or truncate away a negation", () => {
    expect(
      shortShipReason(
        "no profitable executable voyage from calais; provisions and cash needed on an alternative",
      ),
    ).toBe("No profitable route found.");
    expect(
      shortShipReason(
        "Very long technical constraint that does not establish any complete short plain-language sentence or permit safe truncation",
      ),
    ).toBe("Reason recorded; open details.");
    expect(
      shortShipReason("A complete recorded reason. A later technical detail."),
    ).toBe("A complete recorded reason.");
  });
});

describe("recorded passenger occupancy", () => {
  it("distinguishes explicit zero, absent occupancy and occupied hold without groups", () => {
    expect(shipPassengers(account(), detail, now).text).toBe("Passengers 0");
    expect(
      shipPassengers(
        account(),
        { ...detail, ship: { ...detail.ship, passenger_hold_tons: undefined } },
        now,
      ).text,
    ).toBe("Passengers not recorded");
    expect(
      shipPassengers(
        account(),
        { ...detail, ship: { ...detail.ship, passenger_hold_tons: 7 } },
        now,
      ).text,
    ).toBe("Passengers recorded; count unavailable");
    expect(
      shipPassengers(
        account(),
        {
          ...detail,
          passenger_hold_tons: 7,
          ship: { ...detail.ship, passenger_hold_tons: undefined },
        },
        now,
      ).text,
    ).toBe("Passengers recorded; count unavailable");
    expect(
      shipPassengers(account(), { ...detail, passenger_hold_tons: 7 }, now)
        .text,
    ).toBe("Passengers 0");
  });
  it("counts ship-keyed aboard groups, never offers or waiting passengers", () => {
    const value = account();
    value.snapshot.passenger_aboard = {
      ship: [group, { ...group, id: "second", size: 3 }],
    };
    value.snapshot.passenger_waiting = { calais: [{ size: 100 }] };
    value.snapshot.passenger_offers = [{ max_berths: 50 }];
    (value.snapshot._meta as Record<string, unknown>)["passenger_aboard/ship"] =
      meta;
    const result = shipPassengers(
      value,
      { ...detail, ship: { ...detail.ship, passenger_hold_tons: 10 } },
      now,
    );
    expect(result.count).toBe(10);
    expect(result.text).toBe("Passengers 10");
    expect(result.groups).toHaveLength(2);
  });
  it("labels retained groups rather than claiming a current passenger count", () => {
    const value = account();
    value.snapshot.passenger_aboard = { ship: [group] };
    expect(
      shipPassengers(
        value,
        { ...detail, ship: { ...detail.ship, passenger_hold_tons: 7 } },
        now,
      ).text,
    ).toBe("Last recorded passengers 7");
    expect(shipPassengers(value, detail, now).text).toBe("Passengers 0");
  });
  it.each([
    { ...meta, generation: "different" },
    { ...meta, updated_at: new Date(now - 60_000).toISOString() },
    { ...meta, updated_at: new Date(now - 180_000).toISOString() },
  ])(
    "does not promote independent or aging aboard captures to current (%j)",
    (aboardMeta) => {
      const value = account();
      value.snapshot.passenger_aboard = { ship: [group] };
      (value.snapshot._meta as Record<string, unknown>)[
        "passenger_aboard/ship"
      ] = aboardMeta;
      const result = shipPassengers(
        value,
        { ...detail, ship: { ...detail.ship, passenger_hold_tons: 7 } },
        now,
      );
      expect(result.text).toBe("Last recorded passengers 7");
      expect(result.note).toContain("current count is unavailable");
    },
  );
  it("does not make a malformed or null ship entry into zero", () => {
    const value = account();
    for (const raw of [
      null,
      [{ size: -1 }],
      [{ ...group, size: Number.NaN }],
    ]) {
      value.snapshot.passenger_aboard = { ship: raw };
      expect(
        shipPassengers(
          value,
          { ...detail, ship: { ...detail.ship, passenger_hold_tons: 7 } },
          now,
        ).count,
      ).toBeUndefined();
    }
  });
});
