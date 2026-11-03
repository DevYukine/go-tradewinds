import { describe, expect, it } from "vitest";
import { fleet } from "./data";
import { shipOperation } from "./operations";
import type { Account, ShipDetail, ShipLoss } from "./types";

const observedAt = "2026-11-02T12:00:00Z";
const lossAt = "2026-11-03T08:00:00Z";
const refreshedAt = "2026-11-03T09:00:00Z";
const ship: ShipDetail = {
  ship: {
    id: "lapras",
    company_id: "company-a",
    name: "Lapras",
    ship_class: "hoy",
    status: "sailing",
    current_port_id: null,
    current_leg_idx: 0,
    cannons_count: 2,
    stores_provisions_units: 200_000,
    updated_at: observedAt,
  },
  hold_capacity_tons: 40,
  effective_hold_capacity_tons: 36,
  cargo_used_tons: 20,
  cargo_manifest: { grain: 20 },
  stores: { provisions_tons: 1, sea_days_remaining: 10 },
  route_legs: [],
};
const loss: ShipLoss = {
  account_id: "account-a",
  ship_id: "lapras",
  ship_name: "Lapras",
  ship_class: "hoy",
  fate: "lost_at_sea",
  status: "sunk",
  observed_at: lossAt,
  last_observed_at: observedAt,
  recovered_at: null,
  last_port_id: null,
  last_sea_zone_id: "north_sea",
  voyage_origin_id: "london",
  voyage_destination_id: "hamburg",
  cargo_manifest: { grain: 20 },
  stores: { provisions_tons: 1, sea_days_remaining: 10 },
  hazard_cause: "storm",
  insurance_claim_paid_pence: null,
};

function account(
  snapshot: Account["snapshot"],
  shipLosses?: ShipLoss[],
): Account {
  return {
    id: "account-a",
    label: "Yukine",
    company_id: "company-a",
    enabled: true,
    created_at: observedAt,
    snapshot,
    ship_losses: shipLosses,
  };
}

describe("recorded fleet loss retention", () => {
  it("keeps a missing hull visible without inventing current measurements", () => {
    const [detail] = fleet(account({ ships: [] }, [loss]));
    expect(detail.ship).toMatchObject({
      id: "lapras",
      name: "Lapras",
      ship_class: "hoy",
      status: "sunk",
      fate: "lost_at_sea",
      current_port_id: null,
      current_leg_idx: null,
      cannons_count: null,
      stores_provisions_units: null,
      updated_at: null,
    });
    expect(detail.loss).toEqual(loss);
    expect(detail.hold_capacity_tons).toBeNull();
    expect(detail.effective_hold_capacity_tons).toBeNull();
    expect(detail.cargo_used_tons).toBeNull();
    expect(detail.cargo_manifest).toBeNull();
    expect(detail.stores).toBeNull();
    expect(detail.voyage).toBeNull();
  });

  it("keeps absent prior observations unknown rather than an empty cargo loss", () => {
    const missing: ShipLoss = {
      ...loss,
      fate: "missing",
      last_observed_at: null,
      last_port_id: null,
      last_sea_zone_id: null,
      voyage_origin_id: null,
      voyage_destination_id: null,
      cargo_manifest: null,
      stores: null,
      hazard_cause: null,
    };
    const [detail] = fleet(account({}, [missing]));
    expect(detail.ship.status).toBe("sunk");
    expect(detail.ship.fate).toBe("missing");
    expect(detail.loss?.last_observed_at).toBeNull();
    expect(detail.loss?.cargo_manifest).toBeNull();
    expect(detail.loss?.stores).toBeNull();
    expect(detail.ship.current_port_id).toBeNull();
  });

  it("joins history once and preserves the current recorded loss fate", () => {
    const captured = {
      ...ship,
      ship: { ...ship.ship, status: "sunk", fate: "captured" },
    };
    const value = account({ ships: [captured, captured] }, [loss, loss]);
    const details = fleet(value);
    expect(details).toHaveLength(1);
    expect(details[0].ship.status).toBe("sunk");
    expect(details[0].ship.fate).toBe("captured");
    expect(details[0].loss).toEqual(loss);
    expect(captured).not.toHaveProperty("loss");
  });

  it.each([undefined, null, "unknown"])(
    "uses the durable fate when a sunk game row has no known fate (%s)",
    (fate) => {
      const sunk = { ...ship, ship: { ...ship.ship, status: "sunk", fate } };
      const captured = { ...loss, fate: "captured" };
      const [detail] = fleet(account({ ships: [sunk] }, [captured]));
      expect(detail.ship.fate).toBe("captured");
      expect(detail.loss).toEqual(captured);
      expect(sunk.ship.fate).toBe(fate);
    },
  );

  it.each([undefined, null, "unknown"])(
    "does not turn a real sunk observation into an inferred absence (fate=%s)",
    (fate) => {
      const sunk = {
        ...ship,
        ship: { ...ship.ship, status: "sunk", fate, updated_at: refreshedAt },
      };
      const absence = { ...loss, fate: "missing" };
      const value = account(
        {
          ships: [sunk],
          _meta: { ships: { updated_at: refreshedAt, complete: true } },
        },
        [absence],
      );
      const [detail] = fleet(value);
      expect(detail.ship.fate).toBe(fate);
      expect(
        shipOperation(value, detail, Date.parse(refreshedAt)).activity,
      ).toBe("Sunk");
      expect(detail.loss).toBeUndefined();
      expect(value.ship_losses).toEqual([absence]);
    },
  );

  it("does not let historical loss override a newer afloat fleet observation", () => {
    const value = account(
      {
        ships: [ship],
        _meta: { ships: { updated_at: refreshedAt, complete: true } },
      },
      [loss],
    );
    const [detail] = fleet(value);
    expect(detail.ship.status).toBe("sailing");
    expect(detail.ship.fate).toBeUndefined();
    expect(detail.cargo_manifest).toEqual({ grain: 20 });
    expect(detail.loss).toEqual(loss);
    expect(ship.ship.status).toBe("sailing");
  });

  it.each([true, false])(
    "uses a newer afloat inspector observation even when the loss list omits the hull (listed=%s)",
    (listed) => {
      const sunk = { ...ship, ship: { ...ship.ship, status: "sunk" } };
      const value = account(
        {
          ships: listed ? [sunk] : [],
          ship_details: { lapras: ship },
          _meta: {
            ships: { updated_at: lossAt },
            "ship_details/lapras": { updated_at: refreshedAt },
          },
        },
        [loss],
      );
      const [detail] = fleet(value);
      expect(detail.ship.status).toBe("sailing");
      expect(detail.loss).toEqual(loss);
    },
  );

  it("does not leave an older afloat snapshot sailing after a recorded loss", () => {
    const [detail] = fleet(
      account(
        {
          ships: [ship],
          _meta: { ships: { updated_at: observedAt } },
        },
        [loss],
      ),
    );
    expect(detail.ship.status).toBe("sunk");
    expect(detail.ship.fate).toBe("lost_at_sea");
    expect(detail.loss).toEqual(loss);
    expect(ship.ship.status).toBe("sailing");
  });

  it.each([lossAt, "invalid", null])(
    "requires an observation strictly newer than the loss before restoring afloat status (%s)",
    (updatedAt) => {
      const undatedShip = { ...ship, ship: { ...ship.ship, updated_at: null } };
      const [detail] = fleet(
        account(
          {
            ships: [undatedShip],
            _meta: { ships: { updated_at: updatedAt } },
          },
          [loss],
        ),
      );
      expect(detail.ship.status).toBe("sunk");
      expect(detail.ship.fate).toBe("lost_at_sea");
    },
  );

  it("does not treat the game's update timestamp as a newer recording", () => {
    const futureGameRecord = {
      ...ship,
      ship: { ...ship.ship, updated_at: "2036-11-03T09:00:00Z" },
    };
    const [detail] = fleet(account({ ships: [futureGameRecord] }, [loss]));
    expect(detail.ship.status).toBe("sunk");
    expect(detail.ship.fate).toBe("lost_at_sea");
    expect(detail.loss).toEqual(loss);
  });

  it("clears current voyage and docking state when loss history supersedes an older afloat observation", () => {
    const afloat: ShipDetail = {
      ...ship,
      ship: {
        ...ship.ship,
        current_port_id: "london",
        turnaround_unlocks_at: refreshedAt,
        dock_queued_at: observedAt,
        leg_departs_at: observedAt,
        leg_arrives_at: refreshedAt,
        berthed_at: observedAt,
        convoy_id: "convoy-a",
      },
      route_legs: [
        {
          from_node: "london",
          to_node: "hamburg",
          distance_nm: 300,
          edge_class: "open_sea",
          is_sound_toll: false,
        },
      ],
      voyage: {
        current_leg_idx: 0,
        speed_centiknots: 500,
        estimated_arrival_at: refreshedAt,
        legs: [],
      },
    };
    const [detail] = fleet(
      account(
        {
          ships: [afloat],
          _meta: { ships: { updated_at: observedAt } },
        },
        [loss],
      ),
    );
    expect(detail.ship).toMatchObject({
      status: "sunk",
      current_port_id: null,
      current_leg_idx: null,
      turnaround_unlocks_at: null,
      dock_queued_at: null,
      leg_departs_at: null,
      leg_arrives_at: null,
      berthed_at: null,
      convoy_id: null,
      stores_provisions_units: null,
    });
    expect(detail.route_legs).toEqual([]);
    expect(detail.voyage).toBeNull();
    expect(detail.cargo_used_tons).toBeNull();
    expect(detail.cargo_manifest).toBeNull();
    expect(detail.stores).toBeNull();
    expect(detail.hold_capacity_tons).toBe(40);
    expect(detail.ship.cannons_count).toBe(2);
    expect(afloat.ship.current_port_id).toBe("london");
    expect(afloat.voyage).not.toBeNull();
    expect(detail.loss?.voyage_origin_id).toBe("london");
    expect(detail.loss?.cargo_manifest).toEqual({ grain: 20 });
  });

  it("accepts existing accounts without a loss array", () => {
    expect(fleet(account({ ships: [ship] }))).toEqual([ship]);
    expect(fleet(undefined)).toEqual([]);
  });
});

describe("resolved missing-hull observations", () => {
  const recovered: ShipLoss = {
    ...loss,
    fate: "missing",
    recovered_at: refreshedAt,
  };

  it("attaches a recovered absence without forcing an existing hull sunk", () => {
    const [detail] = fleet(account({ ships: [ship] }, [recovered]));
    expect(detail.ship.status).toBe("sailing");
    expect(detail.ship.fate).toBeUndefined();
    expect(detail.cargo_manifest).toEqual({ grain: 20 });
    expect(detail.loss?.recovered_at).toBe(refreshedAt);
  });

  it.each([true, false])(
    "does not reuse a recovered absence for a new terminal observation (listed=%s)",
    (listed) => {
      const sunk = {
        ...ship,
        ship: {
          ...ship.ship,
          status: "sunk",
          fate: "captured",
          current_port_id: "hamburg",
        },
      };
      const details = fleet(
        account(
          {
            ships: listed ? [sunk] : [],
            ship_details: { lapras: sunk },
            _meta: {
              ships: { updated_at: lossAt },
              "ship_details/lapras": { updated_at: refreshedAt },
            },
          },
          [recovered],
        ),
      );
      expect(details).toHaveLength(1);
      expect(details[0].ship.fate).toBe("captured");
      expect(details[0].ship.current_port_id).toBe("hamburg");
      expect(details[0].loss).toBeUndefined();
    },
  );

  it("does not synthesize a lost hull from a resolved absence", () => {
    expect(fleet(account({ ships: [] }, [recovered]))).toEqual([]);
    expect(fleet(account({}, [recovered]))).toEqual([]);
  });
});
