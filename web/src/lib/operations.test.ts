import { describe, expect, it } from "vitest";
import { companyMoney, controllerStatus, shipOperation } from "./operations";
import type { Account, DashboardData, Settlement, ShipDetail } from "./types";

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

describe("operational evidence boundaries", () => {
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
