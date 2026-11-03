import { describe, expect, it } from "vitest";
import {
  cargoEntries,
  companyMoney,
  controllerStatus,
  shipCargo,
  shipOperation,
  shipStatusLabel,
} from "./operations";
import type {
  Account,
  Catalog,
  DashboardData,
  Settlement,
  ShipDetail,
  ShipLoss,
} from "./types";

const observedAt = "2026-10-24T00:00:00.000Z";
const now = Date.parse(observedAt) + 5_000;
const ship: ShipDetail = {
  ship: {
    id: "lapras",
    company_id: "company-a",
    name: "Lapras",
    ship_class: "hoy",
    status: "sailing",
    current_port_id: null,
    current_leg_idx: 0,
    cannons_count: 4,
    stores_provisions_units: 2_000_000,
    updated_at: observedAt,
  },
  hold_capacity_tons: 40,
  effective_hold_capacity_tons: 30,
  cargo_used_tons: 20,
  cargo_manifest: { grain: 20 },
  stores: { provisions_tons: 2, sea_days_remaining: 20 },
  route_legs: [],
};

function account(): Account {
  const meta = {
    generation: "capture-a",
    complete: true,
    updated_at: observedAt,
  };
  return {
    id: "account-a",
    label: "Yukine",
    company_id: "company-a",
    enabled: true,
    created_at: observedAt,
    snapshot: {
      ships: [ship],
      _engine: {
        mode: "execute",
        generation: "capture-a",
        observed_at: observedAt,
        next_check_at: "2026-10-24T00:00:10.000Z",
        assessed_ship_ids: ["lapras", "charizard"],
        holds: [
          { ship_id: "", reason: "Growth deferred: no spare working capital" },
          { ship_id: "lapras", reason: "Authoritative voyage still in flight" },
          { ship_id: "charizard", reason: "Stores need replenishing" },
        ],
      },
      _meta: { _engine: { ...meta }, ships: { ...meta } },
    },
  };
}

function settlement(id: string, changes: Partial<Settlement>): Settlement {
  return {
    id,
    account_id: "account-a",
    ship_id: "lapras",
    mission_id: id,
    lane: "london|dover|grain",
    action_id: id,
    kind: "spot",
    origin: "london",
    destination: "dover",
    commodity: "grain",
    quantity_tons: 20,
    game_seconds: 86_400,
    costs_known: true,
    acquisition_pence: 1_000,
    proceeds_pence: 2_000,
    fees_pence: 100,
    provisions_pence: 50,
    expenses_pence: 50,
    started_at: observedAt,
    completed_at: observedAt,
    created_at: observedAt,
    ...changes,
  };
}

describe("ship fate labels", () => {
  it.each([
    ["lost_at_sea", "Sunk"],
    ["captured", "Captured"],
    ["seized_smuggling", "Seized for smuggling"],
    ["seized_bailiff", "Seized by the bailiff"],
    ["seized_bankruptcy", "Seized after bankruptcy"],
    ["broken_up", "Broken up"],
    ["royal_navy", "Commissioned to the Royal Navy"],
    ["missing", "Missing from fleet list"],
    ["unknown", "Sunk"],
    [undefined, "Sunk"],
    [null, "Sunk"],
    ["", "Sunk"],
    ["unknown_fate", "Gone"],
  ])("shows %s as %s in recorded ship activity", (fate, label) => {
    const detail = { ...ship, ship: { ...ship.ship, status: "sunk", fate } };
    expect(shipOperation(account(), detail, now).activity).toBe(label);
    expect(shipOperation(account(), detail, now + 3_600_000).activity).toBe(
      label,
    );
  });

  it.each([
    ["sailing", "Sailing"],
    ["berthed", "Berthed"],
    ["anchored_roadstead", "Anchored roadstead"],
    ["building", "Building"],
  ])("preserves the normal %s status label", (status, label) => {
    expect(shipStatusLabel(status)).toBe(label);
    expect(shipStatusLabel(status, "captured")).toBe(label);
  });
});

describe("retained loss operational descriptions", () => {
  it.each([
    [null, "At sea near North Sea approaches (approximate)"],
    ["london", "Last afloat: London docks"],
  ])(
    "uses the known berth before approximate sea context (last port=%s)",
    (lastPort, location) => {
      const loss: ShipLoss = {
        account_id: "account-a",
        ship_id: "lapras",
        ship_name: "Lapras",
        ship_class: "hoy",
        status: "sunk",
        fate: "lost_at_sea",
        observed_at: observedAt,
        last_observed_at: observedAt,
        recovered_at: null,
        last_port_id: lastPort,
        last_sea_zone_id: "north_sea",
        voyage_origin_id: "london",
        voyage_destination_id: "hamburg",
        cargo_manifest: { grain: 12, timber: 0 },
        stores: { provisions_tons: 1.5, sea_days_remaining: 8 },
        hazard_cause: "storm",
        insurance_claim_paid_pence: null,
      };
      const lost = {
        ...ship,
        ship: { ...ship.ship, status: "sunk", fate: "lost_at_sea" },
        loss,
      };
      const value = account();
      value.snapshot.catalog = {
        ports: [{ id: "london", name: "London docks" }],
        sea_zones: [{ id: "north_sea", name: "North Sea approaches" }],
        commodities: [],
      };
      const operation = shipOperation(value, lost, now);
      expect(operation.route).toBe(location);
      if (lastPort === null) expect(operation.route).not.toContain("London");
      expect(operation.cargo).toBe("Last afloat: 12 t Grain");
      expect(operation.cargoItems).toEqual([
        { id: "grain", name: "Grain", tons: 12 },
      ]);
      expect(operation.cargo).not.toContain("Timber");
      expect(operation.holdsCurrent).toBe(false);
      expect(operation.starving).toBe(false);
      const empty = shipOperation(
        value,
        { ...lost, loss: { ...loss, cargo_manifest: {} } },
        now,
      );
      expect(empty.cargo).toBe("Last afloat: No trade cargo recorded");
      expect(empty.cargoItems).toEqual([]);
      const unavailable = shipOperation(
        account(),
        {
          ...lost,
          loss: {
            ...loss,
            last_port_id: null,
            last_sea_zone_id: null,
            cargo_manifest: null,
          },
        },
        now,
      );
      expect(unavailable.route).toBe("Last afloat location not recorded");
      expect(unavailable.cargo).toBe("Last afloat cargo not recorded");
      expect(unavailable.cargoItems).toBeUndefined();
    },
  );
});

describe("shared ship cargo", () => {
  it("names and orders the full manifest but limits compact copy to two goods", () => {
    const catalog: Catalog = {
      ports: [],
      sea_zones: [],
      ship_classes: [],
      commodities: [
        {
          id: "grain",
          name: "Bread grain",
          category: "raw",
          base_price_pence: 100,
        },
        {
          id: "provisions",
          name: "Ship stores",
          category: "stores",
          base_price_pence: 100,
        },
      ],
    };
    const loaded = {
      ...ship,
      cargo_manifest: {
        wool: 2,
        timber: 12,
        provisions: 30,
        grain: 12,
        zero: 0,
      },
    };
    const expected = {
      items: [
        { id: "provisions", name: "Ship stores", tons: 30 },
        { id: "grain", name: "Bread grain", tons: 12 },
        { id: "timber", name: "Timber", tons: 12 },
        { id: "wool", name: "Wool", tons: 2 },
      ],
      text: "Cargo 30 t Ship stores, 12 t Bread grain +2 goods",
    };
    expect(shipCargo(loaded, catalog)).toEqual(expected);
    const value = account();
    value.snapshot.catalog = catalog;
    const operation = shipOperation(value, loaded, now);
    expect(operation.cargo).toBe(expected.text);
    expect(operation.cargoItems).toEqual(expected.items);
  });

  it("distinguishes a known empty manifest from missing cargo", () => {
    const manifests: Record<string, number>[] = [{}, { grain: 0 }];
    for (const manifest of manifests) {
      const empty = { ...ship, cargo_manifest: manifest };
      expect(shipCargo(empty)).toEqual({
        items: [],
        text: "No trade cargo recorded",
      });
      expect(shipOperation(account(), empty, now).cargoItems).toEqual([]);
    }
    expect(shipCargo({ ...ship, cargo_manifest: null })).toEqual({
      items: undefined,
      text: "Cargo not recorded",
    });
  });

  it.each([
    { raw: [] },
    { raw: [12] },
    { raw: "grain" },
    { raw: { grain: "12" } },
    { raw: { grain: 12, timber: -1 } },
    { raw: { grain: 12, timber: Number.NaN } },
    { raw: { grain: 12, timber: Number.POSITIVE_INFINITY } },
    { raw: { grain: 12, timber: null } },
  ])(
    "keeps a malformed manifest unknown rather than partly empty (%j)",
    ({ raw }) => {
      const manifest = raw as unknown as ShipDetail["cargo_manifest"];
      const malformed = { ...ship, cargo_manifest: manifest };
      expect(cargoEntries(manifest)).toBeUndefined();
      expect(shipCargo(malformed)).toEqual({
        items: undefined,
        text: "Cargo not recorded",
      });
      const operation = shipOperation(account(), malformed, now);
      expect(operation.cargo).toBe("Cargo not recorded");
      expect(operation.cargoItems).toBeUndefined();
    },
  );

  it("keeps a valid fractional stack and the singular extra-good count", () => {
    expect(
      shipCargo({
        ...ship,
        cargo_manifest: { grain: 2.5, timber: 2, wool: 1 },
      }),
    ).toEqual({
      items: [
        { id: "grain", name: "Grain", tons: 2.5 },
        { id: "timber", name: "Timber", tons: 2 },
        { id: "wool", name: "Wool", tons: 1 },
      ],
      text: "Cargo 2.5 t Grain, 2 t Timber +1 good",
    });
  });
});

describe("operational evidence boundaries", () => {
  it.each([30_000, 119_999])(
    "keeps a delayed but coherent hold assessment usable at %i ms",
    (age) => {
      const value = account();
      const delayedNow = Date.parse(observedAt) + age;
      const operation = shipOperation(value, ship, delayedNow);
      expect(operation.observation.state).toBe("delayed");
      expect(operation.holdsCurrent).toBe(true);
      const meta = value.snapshot._meta as Record<
        string,
        Record<string, unknown>
      >;
      meta.ships.generation = "another-capture";
      expect(shipOperation(value, ship, delayedNow).holdsCurrent).toBe(false);
      expect(
        shipOperation(account(), ship, Date.parse(observedAt) + 120_000)
          .holdsCurrent,
      ).toBe(false);
    },
  );
  it("does not attach an old completed controller assessment to a newer ship capture", () => {
    const value = account();
    expect(shipOperation(value, ship, now).holdsCurrent).toBe(true);
    value.snapshot._meta = {
      ...(value.snapshot._meta as Record<string, unknown>),
      ships: {
        generation: "capture-b",
        complete: true,
        updated_at: observedAt,
      },
    };
    const operation = shipOperation(value, ship, now);
    expect(operation.observation.current).toBe(true);
    expect(operation.holdsCurrent).toBe(false);
    expect(operation.holds.map((hold) => hold.reason)).toEqual([
      "Authoritative voyage still in flight",
    ]);
  });

  it("distinguishes an unavailable hold assessment from an observed empty list", () => {
    const value = account();
    const engine = value.snapshot._engine as Record<string, unknown>;
    delete engine.holds;
    expect(shipOperation(value, ship, now).holdsCurrent).toBe(false);

    engine.holds = null;
    expect(shipOperation(value, ship, now).holdsCurrent).toBe(false);

    engine.holds = [{ ship_id: "lapras" }];
    expect(shipOperation(value, ship, now).holdsCurrent).toBe(false);

    engine.holds = [];
    expect(shipOperation(value, ship, now).holdsCurrent).toBe(true);
  });

  it.each([
    { coverage: undefined },
    { coverage: null },
    { coverage: {} },
    { coverage: "lapras" },
    { coverage: ["lapras", 7] },
    { coverage: ["lapras", ""] },
  ])(
    "does not infer ship readiness from malformed coverage $coverage",
    ({ coverage }) => {
      const value = account();
      const engine = value.snapshot._engine as Record<string, unknown>;
      engine.holds = [];
      engine.assessed_ship_ids = coverage;
      expect(controllerStatus(value, now).assessedShipIDs).toBeUndefined();
      expect(shipOperation(value, ship, now).holdsCurrent).toBe(false);
    },
  );

  it("limits an early-return assessment to visited ships without hiding company holds", () => {
    const value = account();
    const other = {
      ...ship,
      ship: { ...ship.ship, id: "charizard", name: "Charizard" },
    };
    value.snapshot.ships = [ship, other];
    const engine = value.snapshot._engine as Record<string, unknown>;
    engine.assessed_ship_ids = ["lapras"];
    engine.holds = [
      { ship_id: "", reason: "Treasury awaits fresh regional balances" },
    ];
    expect(shipOperation(value, ship, now).holdsCurrent).toBe(true);
    expect(shipOperation(value, other, now).holdsCurrent).toBe(false);
    expect(controllerStatus(value, now).accountHolds).toEqual(engine.holds);

    engine.assessed_ship_ids = [];
    expect(shipOperation(value, ship, now).holdsCurrent).toBe(false);
    expect(shipOperation(value, other, now).holdsCurrent).toBe(false);
    expect(controllerStatus(value, now).accountHolds).toEqual(engine.holds);

    delete engine.assessed_ship_ids;
    expect(controllerStatus(value, now).accountHolds).toEqual(engine.holds);
    expect(shipOperation(value, ship, now).holdsCurrent).toBe(false);
  });

  it("does not let complete controller coverage override stale ship evidence", () => {
    const value = account();
    const meta = value.snapshot._meta as Record<
      string,
      Record<string, unknown>
    >;
    meta.ships.stale = true;
    expect(controllerStatus(value, now).observation.current).toBe(true);
    expect(shipOperation(value, ship, now).holdsCurrent).toBe(false);
  });

  it.each([
    "sourcing modeled exposure 123p exceeds allowance/cap 0p",
    "modeled sourcing costs and protected reserves cannot be funded with observed remittance fees",
  ])(
    "shows the attributed sourcing blocker as the current Overview hold: %s",
    (reason) => {
      const value = account();
      const engine = value.snapshot._engine as Record<string, unknown>;
      engine.assessed_ship_ids = ["lapras"];
      engine.holds = [{ ship_id: "lapras", reason }];
      const operation = shipOperation(value, ship, now);
      expect(operation.holdsCurrent).toBe(true);
      expect(operation.holds).toEqual([{ ship_id: "lapras", reason }]);
    },
  );

  it("keeps account notices separate from a ship's current activity and other ships' holds", () => {
    const value = account();
    const controller = controllerStatus(value, now);
    const operation = shipOperation(value, ship, now);
    expect(controller.accountHolds.map((hold) => hold.reason)).toEqual([
      "Growth deferred: no spare working capital",
    ]);
    expect(operation.activity).toBe("Sailing");
    expect(operation.holds).toEqual([
      { ship_id: "lapras", reason: "Authoritative voyage still in flight" },
    ]);
    expect(shipOperation(value, ship, now + 180_000).holdsCurrent).toBe(false);
  });

  it("does not turn another company's settlements, escrow or unknown costs into this company's earnings", () => {
    const value = account();
    value.snapshot.vaults = {
      vaults: [
        {
          account_type: "vault",
          banking_region: "channel",
          balance_pence: 15_000,
        },
        {
          account_type: "vault",
          banking_region: "iberia",
          balance_pence: -500,
        },
        {
          account_type: "escrow",
          banking_region: "channel",
          balance_pence: 90_000,
        },
      ],
    };
    value.snapshot.leaderboard = {
      you: { is_calling_company: false, net_worth_pence: 1_000_000 },
    };
    const dashboard: DashboardData = {
      updated_at: observedAt,
      accounts: [value],
      metrics: [],
      decisions: [],
      events: [],
      requests: [],
      settlements: [
        settlement("profit", {}),
        settlement("loss", { proceeds_pence: 100 }),
        settlement("unknown", { costs_known: false, proceeds_pence: 80_000 }),
        settlement("passage", {
          kind: "passage",
          event_id: "fare-event",
          proceeds_pence: null,
          proceeds_unknown: true,
          costs_known: false,
        }),
        settlement("other", {
          account_id: "account-b",
          proceeds_pence: 900_000,
        }),
      ],
    };
    const result = companyMoney(value, dashboard);
    expect(result.cashPence).toBe(14_500);
    expect(result.settledNetPence).toBe(-300);
    expect(result.knownSettlements).toBe(2);
    expect(result.unknownSettlements).toBe(2);
    expect(result.netWorthPence).toBeUndefined();
  });
});
