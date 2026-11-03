// @vitest-environment jsdom

import { act, createElement } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type * as Data from "@/lib/data";
import { dateTime, money } from "@/lib/format";
import type { Account, DashboardData, ShipDetail, ShipLoss } from "@/lib/types";
import { useUI } from "@/lib/ui-store";
import { FleetChartPage } from "./fleet-map";

const query = vi.hoisted(() => ({
  data: undefined as DashboardData | undefined,
  dataUpdatedAt: Date.parse("2026-11-03T12:00:00Z"),
  isPending: false,
  isError: false,
}));
vi.mock("@/lib/data", async (original) => ({
  ...(await original<typeof Data>()),
  useDashboard: () => query,
  useWarnings: () => ({ isPending: false, isError: false }),
}));
vi.mock("@/app/providers", () => ({
  useNow: () => Date.parse("2026-11-03T12:00:00Z"),
}));
vi.mock("next/dynamic", () => ({ default: () => () => null }));

const observedAt = "2026-11-03T12:00:00Z";
const detail: ShipDetail = {
  ship: {
    id: "lapras",
    company_id: "company-a",
    name: "Lapras",
    ship_class: "hoy",
    status: "berthed",
    current_port_id: "hamburg",
    current_leg_idx: null,
    cannons_count: null,
    stores_provisions_units: null,
    updated_at: observedAt,
  },
  hold_capacity_tons: 40,
  effective_hold_capacity_tons: 40,
  cargo_used_tons: 7,
  cargo_manifest: { sugar: 7 },
  stores: { provisions_tons: 2, sea_days_remaining: 9 },
  route_legs: [],
};
const loss: ShipLoss = {
  account_id: "account-a",
  ship_id: "lapras",
  ship_name: "Lapras",
  ship_class: "hoy",
  fate: "lost_at_sea",
  status: "sunk",
  observed_at: "2026-11-03T11:30:00Z",
  last_observed_at: "2026-11-03T11:00:00Z",
  recovered_at: null,
  last_port_id: null,
  last_sea_zone_id: "north_sea",
  voyage_origin_id: "london",
  voyage_destination_id: "hamburg",
  cargo_manifest: { grain: 12, timber: 0 },
  stores: { provisions_tons: 1.5, sea_days_remaining: 8 },
  hazard_cause: "storm",
  insurance_claim_paid_pence: 12_000,
};

function account(ships: ShipDetail[], losses: ShipLoss[]): Account {
  return {
    id: "account-a",
    label: "Yukine",
    company_id: "company-a",
    enabled: true,
    created_at: "2026-10-24T00:00:00Z",
    snapshot: {
      ships,
      _meta: { ships: { updated_at: observedAt, complete: true } },
    },
    ship_losses: losses,
  };
}

function dashboard(recorded: Account): DashboardData {
  return {
    updated_at: observedAt,
    accounts: [recorded],
    metrics: [],
    decisions: [
      {
        id: 1,
        account_id: recorded.id,
        ship_id: "lapras",
        kind: "buy_and_depart",
        reason: "Historical trade plan",
        details: { commodity: "sugar", quantity_tons: 7 },
        created_at: "2026-11-03T10:00:00Z",
      },
    ],
    events: [],
    requests: [],
    settlements: [],
  };
}

function facts(section: Element) {
  return Object.fromEntries(
    [...section.querySelectorAll("dl > div")].map((row) => [
      row.querySelector("dt")!.textContent,
      row.querySelector("dd")!.textContent,
    ]),
  );
}

describe("retained vessels on the standalone chart", () => {
  let root: Root;
  let container: HTMLDivElement;

  beforeEach(() => {
    vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
    vi.stubGlobal(
      "requestAnimationFrame",
      vi.fn(() => 1),
    );
    window.history.replaceState(null, "", "/chart");
    useUI.setState({
      accountID: "",
      selectedShipID: "",
      selectedShipAccountID: "",
      selectedPortID: "",
    });
    container = document.createElement("div");
    document.body.append(container);
    root = createRoot(container);
  });

  afterEach(async () => {
    await act(async () => root.unmount());
    container.remove();
    vi.unstubAllGlobals();
  });

  async function render(recorded: Account, shipID = "") {
    query.data = dashboard(recorded);
    useUI.setState({
      selectedShipID: shipID,
      selectedShipAccountID: shipID ? recorded.id : "",
    });
    await act(async () => root.render(createElement(FleetChartPage)));
  }

  function rosterRow(shipID: string) {
    return container.querySelector<HTMLButtonElement>(
      `[data-vessel-key="account-a:${shipID}"]`,
    )!;
  }

  function inspector() {
    return container.querySelector('[aria-label="Ship inspector"]')!;
  }

  it("retains every hull but separates voyage losses from exits and absences", async () => {
    const fates = [
      ["lost_at_sea", "Sunk"],
      ["captured", "Captured"],
      ["missing", "Missing from fleet list"],
      ["broken_up", "Broken up"],
      ["royal_navy", "Commissioned to the Royal Navy"],
      ["seized_smuggling", "Seized for smuggling"],
    ];
    await render(
      account(
        [],
        fates.map(([fate]) => ({
          ...loss,
          ship_id: fate,
          ship_name: fate,
          fate,
        })),
      ),
    );
    expect(container.querySelectorAll("[data-vessel-key]")).toHaveLength(6);
    for (const [fate, label] of fates) {
      expect(rosterRow(fate).textContent).toContain(label);
      expect(rosterRow(fate).textContent).toContain(
        "At sea near North sea (approximate)",
      );
      expect(rosterRow(fate).textContent).toContain("Last afloat: 12 t Grain");
      expect(rosterRow(fate).textContent).not.toContain("0 of");
    }
    const stateFilter = [...container.querySelectorAll("label")]
      .find((label) => label.textContent?.includes("Filter vessel state"))!
      .querySelector("select")!;
    expect(
      [...stateFilter.options].map((option) => option.textContent),
    ).toEqual([
      "All states",
      "Broken up",
      "Captured",
      "Commissioned to the Royal Navy",
      "Missing from fleet list",
      "Seized for smuggling",
      "Sunk",
    ]);
    const lossGroup = [...container.querySelectorAll("h3")].find((heading) =>
      heading.textContent?.startsWith("Recorded losses"),
    )!.parentElement!;
    expect(lossGroup.querySelectorAll("[data-vessel-key]")).toHaveLength(2);
    expect(lossGroup.textContent).toContain("Sunk");
    expect(lossGroup.textContent).toContain("Captured");
    expect(lossGroup.textContent).not.toContain("Broken up");
    expect(lossGroup.textContent).not.toContain("Missing from fleet list");
    expect(lossGroup.textContent).not.toContain("Commissioned");
  });

  it("does not count retained losses as ships needing fresh records", async () => {
    const retained = { ...account([], [loss]), snapshot: { ships: [] } };
    await render(retained);
    expect(
      container.querySelector('[role="status"]')!.textContent,
    ).not.toContain("need updated records");
    expect(
      container.querySelector("[data-error]")!.getAttribute("data-error"),
    ).toBe("false");

    const active = {
      ...detail,
      ship: { ...detail.ship, id: "active", name: "Mew" },
    };
    query.data = dashboard({
      ...retained,
      snapshot: { ships: [active] },
    });
    await act(async () => root.render(createElement(FleetChartPage)));
    expect(container.querySelector('[role="status"]')!.textContent).toContain(
      "1 need updated records",
    );
    expect(
      container.querySelector("[data-error]")!.getAttribute("data-error"),
    ).toBe("true");
  });

  it.each([
    ["broken_up", "Broken up", "Exit record", "exit"],
    ["royal_navy", "Commissioned to the Royal Navy", "Exit record", "exit"],
    ["seized_smuggling", "Seized for smuggling", "Exit record", "exit"],
    ["missing", "Missing from fleet list", "Absence record", "absence"],
  ])(
    "describes retained %s evidence without voyage-loss claims",
    async (fate, label, title, kind) => {
      await render(
        account([], [{ ...loss, fate, last_port_id: "london" }]),
        "lapras",
      );
      const panel = inspector();
      const section = [...panel.querySelectorAll("h3")]
        .find((heading) => heading.textContent === title)!
        .closest("section")!;
      expect(facts(section)).toMatchObject({
        "Recorded fate": label,
        [`First observed ${kind}`]: dateTime(loss.observed_at),
        "Last afloat port": "London",
      });
      expect(panel.textContent?.toLowerCase()).not.toContain("loss");
      expect(panel.textContent).not.toContain("wreck");
      expect(rosterRow("lapras").textContent).toContain(label);
    },
  );

  it("shows optional evidence unavailability reasons as plain text in the retained record", async () => {
    const cargoReason = '<img src=x onerror="alert(1)">';
    await render(
      account(
        [],
        [
          {
            ...loss,
            cargo_manifest: null,
            stores: null,
            hazard_cause: null,
            cargo_unavailable_reason: cargoReason,
            stores_unavailable_reason:
              "Stores evidence exceeded the retention limit.",
            hazard_unavailable_reason: "Hazard evidence was invalid.",
          },
        ],
      ),
      "lapras",
    );
    const panel = inspector();
    expect(facts(panel)).toMatchObject({
      "Last afloat cargo": `Unavailable: ${cargoReason}`,
      "Last afloat stores":
        "Unavailable: Stores evidence exceeded the retention limit.",
      "Hazard cause": "Unavailable: Hazard evidence was invalid.",
      "Game-reported insurance claim": money(loss.insurance_claim_paid_pence),
    });
    expect(
      [...panel.querySelectorAll("dd")]
        .find((item) => item.textContent?.includes(cargoReason))!
        .querySelector("img"),
    ).toBeNull();
  });

  it.each([
    {
      lastPort: null,
      location: "At sea near North Sea approaches (approximate)",
    },
    { lastPort: "london", location: "Last afloat: London docks" },
  ])(
    "uses the catalog location without substituting a voyage origin ($lastPort)",
    async ({ lastPort, location }) => {
      const recorded = account([], [{ ...loss, last_port_id: lastPort }]);
      recorded.snapshot.catalog = {
        ports: [{ id: "london", name: "London docks" }],
        sea_zones: [{ id: "north_sea", name: "North Sea approaches" }],
        commodities: [],
        ship_classes: [],
      };
      await render(recorded, "lapras");
      expect(rosterRow("lapras").textContent).toContain(location);
      expect(
        inspector().querySelector('[aria-label="Vessel location"] h3')!
          .textContent,
      ).toBe(location);
    },
  );

  it.each([
    { losses: [] as ShipLoss[], unavailable: true, message: "unavailable" },
    { losses: undefined, unavailable: false, message: "not recorded" },
  ])(
    "keeps current hulls visible when history is $message",
    async ({ losses, unavailable, message }) => {
      await render({
        ...account([detail], []),
        ship_losses: losses,
        ship_losses_unavailable: unavailable,
      });
      expect(container.textContent).toContain(
        `Yukine: Ship loss and exit history ${message}`,
      );
      expect(container.textContent).toContain(
        "retained vessels may be incomplete",
      );
      expect(rosterRow("lapras").textContent).toContain("Berthed");
      expect(
        container.querySelector("[data-error]")!.getAttribute("data-error"),
      ).toBe("true");
      await render(account([detail], []));
      expect(container.textContent).not.toContain(
        "retained vessels may be incomplete",
      );
      expect(
        container.querySelector("[data-error]")!.getAttribute("data-error"),
      ).toBe("false");
    },
  );

  it("shows durable loss facts instead of stale cargo, plans and routes", async () => {
    const sunk: ShipDetail = {
      ...detail,
      ship: { ...detail.ship, status: "sunk", fate: "captured" },
      route_legs: null as unknown as ShipDetail["route_legs"],
    };
    const captured = {
      ...loss,
      fate: "captured",
      last_port_id: "london",
      voyage_origin_id: "boston",
    };
    await render(account([sunk], [captured]), "lapras");
    const panel = inspector();
    expect(
      panel.querySelector('[aria-label="Vessel location"] h3')!.textContent,
    ).toBe("Last afloat: London");
    expect(rosterRow("lapras").textContent).toContain("Last afloat: London");
    expect(facts(panel)).toMatchObject({
      "Recorded fate": "Captured",
      "First observed loss": dateTime(loss.observed_at),
      "Last observed afloat": dateTime(loss.last_observed_at),
      "Last afloat port": "London",
      "Last afloat sea zone": "North sea",
      "Voyage origin": "Boston",
      "Voyage destination": "Hamburg",
      "Hazard cause": "Storm",
      "Last afloat cargo": "12 t Grain",
      "Last afloat stores": "1.5 t provisions; 8 game days at sea",
      "Game-reported insurance claim": money(loss.insurance_claim_paid_pence),
    });
    const headings = [...panel.querySelectorAll("h3")].map(
      (heading) => heading.textContent,
    );
    expect(headings).not.toContain("Current plan");
    expect(headings).not.toContain("Latest decision");
    expect(headings).not.toContain("Cargo and stores");
    expect(headings).not.toContain("Recorded route");
    expect(panel.textContent).not.toContain("Sugar");
    expect(panel.textContent).toContain("not an exact loss time");
    expect(panel.textContent).toContain("not confirmed losses");
  });

  it("leaves an at-sea observation unplaced instead of substituting the voyage origin", async () => {
    await render(account([], [loss]), "lapras");
    const panel = inspector();
    expect(
      panel.querySelector('[aria-label="Vessel location"] h3')!.textContent,
    ).toBe("At sea near North sea (approximate)");
    expect(
      panel.querySelector('[aria-label="Vessel location"]')!.textContent,
    ).toContain("Loss location not recorded");
    expect(
      panel.querySelector('[aria-label="Vessel location"]')!.textContent,
    ).toContain("At-sea observations remain unplaced");
    expect(facts(panel)).toMatchObject({
      "Last afloat port": "Not recorded",
      "Voyage origin": "London",
    });
  });

  it("does not replace an at-sea durable observation with a sunk row's stale port", async () => {
    const sunk = {
      ...detail,
      ship: { ...detail.ship, status: "sunk", fate: "lost_at_sea" },
    };
    await render(
      account(
        [sunk],
        [{ ...loss, last_port_id: null, voyage_origin_id: null }],
      ),
      "lapras",
    );
    expect(rosterRow("lapras").textContent).toContain(
      "At sea near North sea (approximate)",
    );
    const panel = inspector();
    expect(
      panel.querySelector('[aria-label="Vessel location"] h3')!.textContent,
    ).toBe("At sea near North sea (approximate)");
    expect(facts(panel)["Last afloat port"]).toBe("Not recorded");
    expect(
      panel.querySelector('[aria-label="Vessel location"]')!.textContent,
    ).toContain("Loss location not recorded");
  });

  it.each([null, 0])(
    "keeps unknown evidence distinct from a %s insurance claim",
    async (claim) => {
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
        insurance_claim_paid_pence: claim,
      };
      await render(account([], [missing]), "lapras");
      const panel = inspector();
      expect(rosterRow("lapras").textContent).toContain("Missing");
      expect(rosterRow("lapras").textContent).toContain(
        "Last afloat location not recorded",
      );
      expect(facts(panel)).toMatchObject({
        "Recorded fate": "Missing from fleet list",
        "First observed absence": dateTime(loss.observed_at),
        "Last observed afloat": "Not recorded",
        "Last afloat cargo": "Not recorded",
        "Last afloat stores": "Not recorded",
        "Game-reported insurance claim": money(claim),
      });
      expect(panel.textContent).not.toContain("0 t");
      if (claim === null) expect(panel.textContent).not.toContain("0.00 Cr");
    },
  );

  it("keeps legacy sunk hulls inspectable without inventing durable facts", async () => {
    const legacy: ShipDetail = {
      ...detail,
      ship: { ...detail.ship, status: "sunk", fate: "unknown" },
      cargo_used_tons: null,
      effective_hold_capacity_tons: null,
      cargo_manifest: null,
      stores: null,
      route_legs: null as unknown as ShipDetail["route_legs"],
    };
    await render(account([legacy], []), "lapras");
    expect(rosterRow("lapras").textContent).toContain(
      "Game-reported last port: Hamburg",
    );
    expect(
      inspector().querySelector('[aria-label="Vessel location"] h3')!
        .textContent,
    ).toBe("Game-reported last port: Hamburg");
    expect(inspector().textContent).toContain(
      "Detailed loss evidence not recorded",
    );
    expect(inspector().textContent).not.toContain("Cargo and stores");
    expect(inspector().textContent).not.toContain("0 t");
  });

  it("leaves a recovered hull afloat with current cargo and a historical absence", async () => {
    await render(
      account(
        [detail],
        [{ ...loss, fate: "missing", recovered_at: observedAt }],
      ),
      "lapras",
    );
    const panel = inspector();
    expect(rosterRow("lapras").textContent).toContain("Berthed");
    expect(rosterRow("lapras").textContent).toContain("Hamburg");
    expect(rosterRow("lapras").textContent).not.toContain("Last voyage origin");
    expect(
      panel.querySelector('[aria-label="Vessel location"] h3')!.textContent,
    ).toBe("Hamburg");
    expect(panel.textContent).toContain("Past absence record");
    expect(panel.textContent).toContain("The ship is in the fleet again");
    expect(panel.textContent).toContain("Cargo and stores");
    expect(panel.textContent).toContain("7 t");
    expect(panel.textContent).not.toContain("A wreck symbol");
  });

  it("does not draw a cargo fill for unknown measurements", async () => {
    const unknown: ShipDetail = {
      ...detail,
      cargo_used_tons: null,
      effective_hold_capacity_tons: null,
    };
    const empty: ShipDetail = {
      ...detail,
      ship: { ...detail.ship, id: "empty", name: "Mew" },
      cargo_used_tons: 0,
      cargo_manifest: {},
    };
    await render(account([unknown, empty], []));
    expect(rosterRow("lapras").textContent).toContain("Cargo use not recorded");
    expect(
      rosterRow("lapras").querySelectorAll('[style*="width"]'),
    ).toHaveLength(0);
    expect(rosterRow("empty").textContent).toContain("0 of 40 t");
  });

  it("preserves a selected lost ship and fate filter when recordings refresh", async () => {
    const captured = { ...loss, fate: "captured" };
    await render(account([], [captured]));
    await act(async () => rosterRow("lapras").click());
    const stateFilter = [...container.querySelectorAll("label")]
      .find((label) => label.textContent?.includes("Filter vessel state"))!
      .querySelector("select")!;
    await act(async () => {
      stateFilter.value = "Captured";
      stateFilter.dispatchEvent(new Event("change", { bubbles: true }));
    });
    query.data = dashboard(
      account(
        [],
        [
          captured,
          { ...loss, ship_id: "missing", ship_name: "Mew", fate: "missing" },
        ],
      ),
    );
    await act(async () => root.render(createElement(FleetChartPage)));
    expect(stateFilter.value).toBe("Captured");
    expect(container.querySelectorAll("[data-vessel-key]")).toHaveLength(1);
    expect(rosterRow("lapras").getAttribute("aria-pressed")).toBe("true");
    expect(useUI.getState().selectedShipID).toBe("lapras");
    expect(inspector().textContent).toContain("Loss record");
  });
});
