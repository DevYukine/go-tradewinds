// @vitest-environment jsdom

import { act, createElement } from "react";
import { createRoot, type Root } from "react-dom/client";
import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fleet } from "@/lib/data";
import { dateTime, money } from "@/lib/format";
import * as shipSummaries from "@/lib/ship-summary";
import type { Account, DashboardData, ShipDetail, ShipLoss } from "@/lib/types";
import { useUI } from "@/lib/ui-store";
import { DecisionPanel } from "./decisions";
import { FleetPanel } from "./fleet";
import { ShipInspector } from "./ship-inspector";
import { OperationsOverview } from "./operations-overview";

vi.mock("@/app/providers", () => ({
  useNow: () => Date.parse("2026-11-01T12:00:00Z"),
}));

const detail: ShipDetail = {
  ship: {
    id: "ship-lapras",
    company_id: "company-a",
    name: "Lapras",
    ship_class: "hoy",
    status: "berthed",
    current_port_id: "london",
    current_leg_idx: 0,
    cannons_count: 0,
    stores_provisions_units: 0,
    updated_at: "2026-11-01T11:00:00Z",
  },
  hold_capacity_tons: 40,
  effective_hold_capacity_tons: 40,
  cargo_used_tons: 0,
  cargo_manifest: {},
  stores: { provisions_tons: 0, sea_days_remaining: 0 },
  route_legs: [],
};

const account: Account = {
  id: "account-a",
  label: "Yukine",
  company_id: "company-a",
  enabled: true,
  created_at: "2026-10-24T00:00:00Z",
  snapshot: { ships: [detail] },
};

const loss: ShipLoss = {
  account_id: account.id,
  ship_id: detail.ship.id,
  ship_name: detail.ship.name,
  ship_class: "hoy",
  fate: "lost_at_sea",
  status: "sunk",
  observed_at: "2026-11-01T11:30:00Z",
  last_observed_at: "2026-11-01T11:00:00Z",
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

function render(value: ShipDetail = detail) {
  const container = document.createElement("div");
  container.innerHTML = renderToStaticMarkup(
    createElement(ShipInspector, {
      account,
      detail: value,
    }),
  );
  return container;
}

describe("ship inspector former names", () => {
  it("shows newest-first former names with the dates they were dropped", () => {
    const formerNames = [
      { name: "Recent name", renamed_at: "2026-11-01T10:30:00Z" },
      { name: "Original name", renamed_at: "2026-10-28T09:15:00Z" },
    ];
    const container = render({ ...detail, former_names: formerNames });
    const heading = [...container.querySelectorAll("h3")].find(
      (element) => element.textContent === "Former names",
    );
    expect(heading).toBeDefined();
    const section = heading!.closest("section")!;
    expect(
      [...section.querySelectorAll("dt")].map((node) => node.textContent),
    ).toEqual(["Recent name", "Original name"]);
    expect(
      [...section.querySelectorAll("dd")].map((node) => node.textContent),
    ).toEqual(
      formerNames.map((former) => `Dropped ${dateTime(former.renamed_at)}`),
    );
  });

  it.each([{ names: undefined }, { names: [] }])(
    "hides former names when absent or empty ($names)",
    ({ names }) => {
      const container = render({ ...detail, former_names: names });
      expect(
        [...container.querySelectorAll("h3")].some(
          (element) => element.textContent === "Former names",
        ),
      ).toBe(false);
      expect(container.textContent).toContain("Lapras");
    },
  );

  it("renders recorded former names as plain text", () => {
    const name = '<img src=x onerror="alert(1)">';
    const container = render({
      ...detail,
      former_names: [{ name, renamed_at: "2026-11-01T10:30:00Z" }],
    });
    const label = [...container.querySelectorAll("dt")].find(
      (element) => element.textContent === name,
    );
    expect(label).toBeDefined();
    expect(label!.querySelector("img")).toBeNull();
  });
});

describe("ship inspector loss evidence", () => {
  it("shows the first loss observation and the last afloat evidence separately", () => {
    const [retained] = fleet({
      ...account,
      snapshot: { ships: [] },
      ship_losses: [loss],
    });
    const container = render(retained);
    const heading = [...container.querySelectorAll("h3")].find(
      (element) => element.textContent === "Loss record",
    );
    expect(heading).toBeDefined();
    const section = heading!.closest("section")!;
    const facts = Object.fromEntries(
      [...section.querySelectorAll("dl > div")].map((row) => [
        row.querySelector("dt")!.textContent,
        row.querySelector("dd")!.textContent,
      ]),
    );
    expect(facts).toMatchObject({
      "Recorded fate": "Sunk",
      "First observed loss": dateTime(loss.observed_at),
      "Last observed afloat": dateTime(loss.last_observed_at),
      "Last afloat port": "Not recorded",
      "Last afloat sea zone": "North sea",
      "Voyage origin": "London",
      "Voyage destination": "Hamburg",
      "Hazard cause": "Storm",
      "Last afloat cargo": "12 t Grain",
      "Last afloat stores": "1.5 t provisions; 8 game days at sea",
      "Game-reported insurance claim": money(loss.insurance_claim_paid_pence),
    });
    expect(section.textContent).toContain("not an exact loss time");
    expect(section.textContent).toContain("not confirmed losses");
    expect(container.textContent).not.toContain("0 t capacity");
    expect(
      [...container.querySelectorAll("h3")].some((element) =>
        element.textContent?.includes("Cargo manifest"),
      ),
    ).toBe(false);
  });

  it("labels unavailable evidence without treating it as zero cargo or payment", () => {
    const missing: ShipLoss = {
      ...loss,
      fate: "missing",
      last_observed_at: null,
      last_sea_zone_id: null,
      voyage_origin_id: null,
      voyage_destination_id: null,
      cargo_manifest: null,
      stores: null,
      hazard_cause: null,
      insurance_claim_paid_pence: null,
    };
    const [retained] = fleet({
      ...account,
      snapshot: {},
      ship_losses: [missing],
    });
    const container = render(retained);
    const heading = [...container.querySelectorAll("h3")].find(
      (element) => element.textContent === "Absence record",
    );
    const section = heading!.closest("section")!;
    const facts = Object.fromEntries(
      [...section.querySelectorAll("dl > div")].map((row) => [
        row.querySelector("dt")!.textContent,
        row.querySelector("dd")!.textContent,
      ]),
    );
    expect(facts).toMatchObject({
      "Recorded fate": "Missing from fleet list",
      "First observed absence": dateTime(loss.observed_at),
      "Last observed afloat": "Not recorded",
      "Last afloat cargo": "Not recorded",
      "Last afloat stores": "Not recorded",
      "Hazard cause": "Not recorded",
      "Game-reported insurance claim": "Not recorded",
    });
    expect(section.textContent).not.toContain("0 t");
    expect(section.textContent).not.toContain("0.00 Cr");
    expect(section.textContent?.toLowerCase()).not.toContain("loss");
  });

  it("distinguishes a recorded zero insurance payment from unavailable payment", () => {
    const container = render({
      ...detail,
      loss: { ...loss, insurance_claim_paid_pence: 0 },
    });
    const insuranceLabel = [...container.querySelectorAll("dt")].find(
      (element) => element.textContent === "Game-reported insurance claim",
    );
    expect(insuranceLabel!.nextElementSibling!.textContent).toBe(money(0));
  });

  it.each([
    ["broken_up", "Broken up"],
    ["royal_navy", "Commissioned to the Royal Navy"],
    ["seized_smuggling", "Seized for smuggling"],
    ["seized_bailiff", "Seized by the bailiff"],
    ["seized_bankruptcy", "Seized after bankruptcy"],
  ])("describes %s as an exit rather than a voyage loss", (fate, label) => {
    const [retained] = fleet({
      ...account,
      snapshot: { ships: [] },
      ship_losses: [{ ...loss, fate }],
    });
    const container = render(retained);
    const section = [...container.querySelectorAll("h3")]
      .find((heading) => heading.textContent === "Exit record")!
      .closest("section")!;
    expect(section.textContent).toContain(label);
    expect(section.textContent).toContain("First observed exit");
    expect(section.textContent).toContain(dateTime(loss.observed_at));
    expect(section.textContent?.toLowerCase()).not.toContain("loss");
    expect(container.querySelector(".lucide-skull")).toBeNull();
  });

  it("explains unavailable optional evidence without hiding the rest of a retained record", () => {
    const [retained] = fleet({
      ...account,
      snapshot: { ships: [] },
      ship_losses: [
        {
          ...loss,
          cargo_manifest: null,
          stores: null,
          hazard_cause: null,
          cargo_unavailable_reason:
            "Cargo evidence exceeded the retention limit.",
          stores_unavailable_reason: "Stores evidence was invalid.",
          hazard_unavailable_reason: "Hazard evidence was invalid.",
        },
      ],
    });
    const container = render(retained);
    const facts = Object.fromEntries(
      [...container.querySelectorAll("dl > div")].map((row) => [
        row.querySelector("dt")!.textContent,
        row.querySelector("dd")!.textContent,
      ]),
    );
    expect(facts).toMatchObject({
      "Recorded fate": "Sunk",
      "Last afloat cargo":
        "Unavailable: Cargo evidence exceeded the retention limit.",
      "Last afloat stores": "Unavailable: Stores evidence was invalid.",
      "Hazard cause": "Unavailable: Hazard evidence was invalid.",
      "Game-reported insurance claim": money(loss.insurance_claim_paid_pence),
    });
  });
});

describe("ship inspector observation boundaries", () => {
  it("qualifies an old loss as historical when a newer afloat observation wins", () => {
    const [afloat] = fleet({
      ...account,
      snapshot: {
        ships: [detail],
        _meta: {
          ships: { updated_at: "2026-11-01T12:00:00Z", complete: true },
        },
      },
      ship_losses: [loss],
    });
    const container = render(afloat);
    expect(afloat.ship.status).toBe("berthed");
    expect(
      [...container.querySelectorAll("h3")].map(
        (heading) => heading.textContent,
      ),
    ).toContain("Historical loss record");
    expect(container.textContent).toContain(
      "A newer afloat observation overrides this historical loss record.",
    );
    expect(container.textContent).toContain(
      "The ship is not currently recorded as lost.",
    );
  });

  it("describes a recovered missing hull as a resolved absence, not a loss", () => {
    const recoveredAt = "2026-11-01T11:45:00Z";
    const [afloat] = fleet({
      ...account,
      ship_losses: [{ ...loss, fate: "missing", recovered_at: recoveredAt }],
    });
    const container = render(afloat);
    expect(afloat.ship.status).toBe("berthed");
    const heading = [...container.querySelectorAll("h3")].find(
      (node) => node.textContent === "Past absence record",
    );
    const section = heading!.closest("section")!;
    expect(section.textContent).toContain("resolved this absence");
    expect(section.textContent).toContain("The ship is in the fleet again");
    const recovered = [...section.querySelectorAll("dt")].find(
      (node) => node.textContent === "Observed afloat again",
    );
    expect(recovered!.nextElementSibling!.textContent).toBe(
      dateTime(recoveredAt),
    );
    expect(section.textContent).not.toContain("First observed loss");
  });

  it("renders unknown measurements without units or a misleading capacity ratio", () => {
    const container = render({
      ...detail,
      ship: { ...detail.ship, cannons_count: null },
      cargo_used_tons: null,
      effective_hold_capacity_tons: 40,
      hold_capacity_tons: null,
      stores: null,
    });
    const facts = Object.fromEntries(
      [...container.querySelectorAll("dl > div")].map((row) => [
        row.querySelector("dt")!.textContent,
        row.querySelector("dd")!.textContent,
      ]),
    );
    expect(facts).toMatchObject({
      "Provisions aboard": "Not recorded",
      "Sea endurance": "Not recorded",
      "Hull hold capacity": "Not recorded",
      "Mounted cannons": "Not recorded",
      "Base catalog speed": "Not recorded",
    });
    expect(container.textContent).toContain("Cargo capacity use not recorded.");
    expect(container.textContent).not.toContain("Not recorded / 40");
    expect(container.textContent).not.toContain("Not recorded t");
    expect(container.textContent).not.toContain("Not recorded game days");
  });
});

describe("retained fleet views", () => {
  const captured: ShipLoss = {
    ...loss,
    ship_id: "captured",
    ship_name: "Charmander",
    fate: "captured",
  };
  const missing: ShipLoss = {
    ...loss,
    ship_id: "missing",
    ship_name: "Squirtle",
    fate: "missing",
    last_observed_at: null,
    last_sea_zone_id: null,
    voyage_origin_id: null,
    voyage_destination_id: null,
    cargo_manifest: null,
    stores: null,
    hazard_cause: null,
  };
  const active: ShipDetail = {
    ...detail,
    ship: { ...detail.ship, id: "active", name: "Bulbasaur" },
    cargo_used_tons: null,
    effective_hold_capacity_tons: null,
    stores: null,
  };
  const legacy: ShipDetail = {
    ...detail,
    ship: {
      ...detail.ship,
      id: "legacy-sunk",
      name: "Gastly",
      status: "sunk",
      fate: "unknown",
    },
  };
  const recorded: Account = {
    ...account,
    snapshot: {
      ships: [active, legacy],
      _meta: {
        ships: { updated_at: "2026-11-01T11:59:55Z", complete: true },
      },
    },
    ship_losses: [loss, captured, missing],
  };
  const dashboard: DashboardData = {
    updated_at: "2026-11-01T12:00:00Z",
    accounts: [recorded],
    metrics: [],
    decisions: [],
    events: [],
    requests: [],
    settlements: [],
  };
  let root: Root | undefined;
  let clientContainer: HTMLDivElement | undefined;

  beforeEach(() => {
    vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
    vi.stubGlobal("matchMedia", () => ({
      matches: false,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    }));
    useUI.setState({ selectedShipID: "", selectedShipAccountID: "" });
  });

  afterEach(async () => {
    if (root) {
      await act(async () => root!.unmount());
      root = undefined;
    }
    clientContainer?.remove();
    clientContainer = undefined;
    vi.unstubAllGlobals();
  });

  it("keeps exits and unresolved absences in the ledger and overview without loss wording", () => {
    const fates = [
      ["broken_up", "Broken up"],
      ["royal_navy", "Commissioned to the Royal Navy"],
      ["missing", "Missing from fleet list"],
    ];
    const exits: Account = {
      ...account,
      snapshot: { ships: [] },
      ship_losses: fates.map(([fate]) => ({
        ...loss,
        ship_id: fate,
        ship_name: fate,
        fate,
        hazard_cause: null,
      })),
    };
    const container = document.createElement("div");
    container.innerHTML = renderToStaticMarkup(
      createElement(FleetPanel, { accounts: [exits], dashboard }),
    );
    const rows = [...container.querySelectorAll("tbody tr")];
    expect(rows).toHaveLength(3);
    for (const [fate, label] of fates) {
      const row = rows.find((item) =>
        item.querySelector("button")?.textContent?.includes(fate),
      )!;
      expect(row.textContent).toContain(label);
      expect(row.textContent).not.toContain("Why:");
      expect(row.textContent).toContain("Last afloat: 12 t Grain");
      expect(row.textContent?.toLowerCase()).not.toContain("loss");
      expect(row.querySelector(".lucide-skull")).toBeNull();
    }
    container.innerHTML = renderToStaticMarkup(
      createElement(OperationsOverview, { accounts: [exits], dashboard }),
    );
    expect(container.querySelectorAll("li")).toHaveLength(3);
    for (const [, label] of fates) {
      const row = [...container.querySelectorAll("li")].find((item) =>
        item.textContent?.includes(label),
      )!;
      expect(row.textContent).not.toContain("Why:");
      expect(row.textContent?.toLowerCase()).not.toContain("loss");
    }
  });

  it.each([
    { losses: [] as ShipLoss[], unavailable: true, message: "unavailable" },
    { losses: undefined, unavailable: false, message: "not recorded" },
  ])(
    "discloses $message history rather than treating it as an empty fleet",
    ({ losses, unavailable, message }) => {
      const incomplete: Account = {
        ...account,
        ship_losses: losses,
        ship_losses_unavailable: unavailable,
      };
      const container = document.createElement("div");
      for (const Component of [FleetPanel, OperationsOverview]) {
        container.innerHTML = renderToStaticMarkup(
          createElement(Component, { accounts: [incomplete], dashboard }),
        );
        expect(container.textContent).toContain(
          `Yukine: Ship loss and exit history ${message}`,
        );
        expect(container.textContent).toContain(
          "retained vessels may be incomplete",
        );
        expect(container.textContent).toContain("Lapras");
        container.innerHTML = renderToStaticMarkup(
          createElement(Component, {
            accounts: [{ ...account, ship_losses: [] }],
            dashboard,
          }),
        );
        expect(container.textContent).not.toContain(
          "retained vessels may be incomplete",
        );
      }
    },
  );

  it.each([
    {
      lastPort: null,
      location: "At sea near North Sea approaches (approximate)",
    },
    { lastPort: "london", location: "Last afloat: London docks" },
  ])(
    "uses catalog names and known berths in fleet and operations locations ($lastPort)",
    ({ lastPort, location }) => {
      const located: Account = {
        ...account,
        snapshot: {
          ships: [],
          catalog: {
            ports: [{ id: "london", name: "London docks" }],
            sea_zones: [{ id: "north_sea", name: "North Sea approaches" }],
            ship_classes: [],
            commodities: [],
          },
        },
        ship_losses: [{ ...loss, last_port_id: lastPort }],
      };
      const container = document.createElement("div");
      for (const Component of [FleetPanel, OperationsOverview]) {
        container.innerHTML = renderToStaticMarkup(
          createElement(Component, { accounts: [located], dashboard }),
        );
        expect(container.textContent).toContain(location);
        if (lastPort) {
          expect(container.textContent).not.toContain("At sea near");
        } else {
          expect(container.textContent).not.toContain("Last afloat: London");
        }
      }
    },
  );

  it("renders retained loss rows and honest last-afloat cells by default", () => {
    const container = document.createElement("div");
    container.innerHTML = renderToStaticMarkup(
      createElement(FleetPanel, { accounts: [recorded], dashboard }),
    );
    expect(container.querySelectorAll("tbody tr")).toHaveLength(5);
    const lapras = [...container.querySelectorAll("tbody tr")].find((row) =>
      row.querySelector("button")?.textContent?.includes("Lapras"),
    )!;
    expect(lapras.textContent).toContain("Sunk");
    expect(lapras.textContent).toContain("Why: Storm.");
    expect(lapras.textContent).toContain("At sea near North sea (approximate)");
    expect(lapras.textContent).toContain("Last afloat: 8 sea days");
    expect(lapras.textContent).toContain("Last afloat: 12 t Grain");
    const legacy = [...container.querySelectorAll("tbody tr")].find((row) =>
      row.querySelector("button")?.textContent?.includes("Gastly"),
    )!;
    expect(legacy.querySelectorAll("td")[2].textContent).toContain("Sunk");
    expect(legacy.querySelectorAll("td")[1].textContent).toBe(
      "Last afloat cargo not recorded",
    );
    expect(legacy.textContent).not.toContain("Passengers 0");
    const missing = [...container.querySelectorAll("tbody tr")].find((row) =>
      row.querySelector("button")?.textContent?.includes("Squirtle"),
    )!;
    expect(missing.textContent).toContain("Missing");
    expect(missing.textContent).toContain("Last afloat stores not recorded");
    expect(container.textContent).not.toContain("Not recorded t");
    expect(container.textContent).not.toContain("Not recorded sea days");
    const status = [...container.querySelectorAll("label")]
      .find((label) =>
        label.textContent?.includes("Status, fate and warnings"),
      )!
      .querySelector("select")!;
    expect([...status.options].map((option) => option.textContent)).toEqual([
      "All recorded states",
      "Berthed",
      "Captured",
      "Missing from fleet list",
      "Sunk",
      "Starvation in voyage estimate",
    ]);
  });

  it("sorts and filters actual fate rows while preserving table state on refresh", async () => {
    clientContainer = document.createElement("div");
    document.body.append(clientContainer);
    root = createRoot(clientContainer);
    await act(async () => {
      root!.render(
        createElement(FleetPanel, { accounts: [recorded], dashboard }),
      );
    });
    const heading = [
      ...clientContainer.querySelectorAll<HTMLButtonElement>("th button"),
    ].find((button) => button.textContent?.includes("Observed activity"))!;
    await act(async () => heading.click());
    expect(
      [...clientContainer.querySelectorAll("tbody tr")].map(
        (row) => row.querySelectorAll("td")[2].textContent,
      ),
    ).toEqual([
      "Berthed in London",
      "Captured",
      "Missing from fleet list",
      "Sunk",
      "Sunk",
    ]);
    const status = [...clientContainer.querySelectorAll("label")]
      .find((label) =>
        label.textContent?.includes("Status, fate and warnings"),
      )!
      .querySelector("select")!;
    await act(async () => {
      status.value = "Captured";
      status.dispatchEvent(new Event("change", { bubbles: true }));
    });
    expect(clientContainer.querySelectorAll("tbody tr")).toHaveLength(1);
    expect(clientContainer.querySelector("tbody")!.textContent).toContain(
      "Charmander",
    );
    expect(clientContainer.querySelector("tbody")!.textContent).not.toContain(
      "Lapras",
    );
    const refreshed: Account = {
      ...recorded,
      ship_losses: [
        ...recorded.ship_losses!,
        { ...missing, ship_id: "new-missing", ship_name: "Mew" },
      ],
    };
    await act(async () => {
      root!.render(
        createElement(FleetPanel, { accounts: [refreshed], dashboard }),
      );
    });
    expect(status.value).toBe("Captured");
    expect(clientContainer.querySelectorAll("tbody tr")).toHaveLength(1);
    expect(
      clientContainer.querySelector('th[aria-sort="ascending"]')?.textContent,
    ).toContain("Observed activity");
    await act(async () => {
      status.value = "Missing from fleet list";
      status.dispatchEvent(new Event("change", { bubbles: true }));
    });
    expect(clientContainer.querySelectorAll("tbody tr")).toHaveLength(2);
    expect(clientContainer.querySelector("tbody")!.textContent).toContain(
      "Squirtle",
    );
    expect(clientContainer.querySelector("tbody")!.textContent).toContain(
      "Mew",
    );
  });

  it("sorts Cargo numerically with unknowns last and keeps Why in its reordered column", async () => {
    const observed = "2026-11-01T11:59:55Z";
    const meta = {
      complete: true,
      generation: "capture",
      updated_at: observed,
    };
    const ships = [
      { name: "Zubat", tons: 2, reason: "insufficient cash for departure" },
      { name: "Abra", tons: 12, reason: "provisions required for departure" },
      { name: "Mew", tons: 100, reason: "repairs required for departure" },
      { name: "Gastly", tons: null, reason: "" },
    ];
    const value: Account = {
      ...account,
      ship_losses: [],
      snapshot: {
        ships: ships.map(({ name, tons }) => ({
          ...detail,
          ship: { ...detail.ship, id: name, name },
          cargo_used_tons: tons,
          cargo_manifest: tons === null ? null : { grain: tons },
        })),
        _engine: {
          generation: "capture",
          observed_at: observed,
          assessed_ship_ids: ships.map(({ name }) => name),
          holds: ships
            .filter(({ reason }) => reason)
            .map(({ name, reason }) => ({ ship_id: name, reason })),
        },
        _meta: { ships: meta, _engine: meta },
      },
    };
    clientContainer = document.createElement("div");
    document.body.append(clientContainer);
    root = createRoot(clientContainer);
    await act(async () =>
      root!.render(createElement(FleetPanel, { accounts: [value], dashboard })),
    );
    const headings = [
      ...clientContainer.querySelectorAll<HTMLButtonElement>("th button"),
    ];
    expect(headings.slice(0, 5).map((node) => node.textContent)).toEqual([
      "Ship",
      "Cargo",
      "Observed activity",
      "Why",
      "Port / destination",
    ]);
    const cargo = headings.find((node) => node.textContent === "Cargo")!;
    for (const [direction, expected] of [
      ["ascending", ["Zubat", "Abra", "Mew", "Gastly"]],
      ["descending", ["Mew", "Abra", "Zubat", "Gastly"]],
    ] as const) {
      await act(async () => cargo.click());
      if (cargo.closest("th")!.getAttribute("aria-sort") !== direction) {
        await act(async () => cargo.click());
      }
      expect(cargo.closest("th")!.getAttribute("aria-sort")).toBe(direction);
      expect(
        [...clientContainer.querySelectorAll("tbody tr")].map(
          (row) =>
            row.querySelector("button > span:last-child")!.firstChild!
              .textContent,
        ),
      ).toEqual(expected);
    }
    const reasons: Record<string, string> = {
      Zubat: "Why: Funding needed.",
      Abra: "Why: Provisions needed.",
      Mew: "Why: Repairs needed.",
      Gastly: "Why: Reason not recorded.",
    };
    for (const row of clientContainer.querySelectorAll("tbody tr")) {
      const name = row.querySelector("button > span:last-child")!.firstChild!
        .textContent!;
      expect(row.querySelectorAll("td")[3].textContent).toBe(reasons[name]);
    }
    const why = headings.find((node) => node.textContent === "Why")!;
    await act(async () => why.click());
    expect(
      clientContainer.querySelector('th[aria-sort="ascending"]')!.textContent,
    ).toBe("Why");
    expect(
      [...clientContainer.querySelectorAll("tbody tr")].map(
        (row) =>
          row.querySelector("button > span:last-child")!.firstChild!
            .textContent,
      ),
    ).toEqual(["Zubat", "Abra", "Gastly", "Mew"]);
  });

  it("returns focus to the current ledger opener when the wide inspector closes", async () => {
    vi.stubGlobal("matchMedia", () => ({
      matches: true,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    }));
    clientContainer = document.createElement("div");
    document.body.append(clientContainer);
    root = createRoot(clientContainer);
    await act(async () =>
      root!.render(
        createElement(FleetPanel, { accounts: [recorded], dashboard }),
      ),
    );
    for (const name of ["Bulbasaur", "Lapras"]) {
      const trigger = clientContainer.querySelector<HTMLButtonElement>(
        `button[aria-label^="Inspect ${name},"]`,
      )!;
      await act(async () => {
        trigger.focus();
        trigger.click();
      });
      const aside = clientContainer.querySelector("aside")!;
      expect(aside).not.toBeNull();
      expect(aside.textContent).toContain(name);
      await act(async () =>
        aside
          .querySelector<HTMLButtonElement>(
            'button[aria-label="Close ship inspector"]',
          )!
          .click(),
      );
      expect(clientContainer.querySelector("aside")).toBeNull();
      expect(useUI.getState().selectedShipID).toBe("");
      expect(document.activeElement).toBe(trigger);
    }
  });

  it("does not treat a gone hull's stale port as a current port", async () => {
    const afloat: ShipDetail = {
      ...active,
      ship: { ...active.ship, current_port_id: "hamburg" },
    };
    const current: Account = {
      ...recorded,
      snapshot: { ...recorded.snapshot, ships: [afloat, legacy] },
    };
    clientContainer = document.createElement("div");
    document.body.append(clientContainer);
    root = createRoot(clientContainer);
    await act(async () => {
      root!.render(
        createElement(FleetPanel, { accounts: [current], dashboard }),
      );
    });
    const ports = [...clientContainer.querySelectorAll("label")]
      .find((label) =>
        label.textContent?.includes("Current or destination port"),
      )!
      .querySelector("select")!;
    expect([...ports.options].map((option) => option.value)).not.toContain(
      "london",
    );
    expect([...ports.options].map((option) => option.value)).toContain(
      "hamburg",
    );
    await act(async () => {
      ports.value = "hamburg";
      ports.dispatchEvent(new Event("change", { bubbles: true }));
    });
    expect(clientContainer.querySelectorAll("tbody tr")).toHaveLength(1);
    expect(clientContainer.querySelector("tbody")!.textContent).toContain(
      "Bulbasaur",
    );
    expect(clientContainer.querySelector("tbody")!.textContent).not.toContain(
      "Gastly",
    );
  });

  it("keeps lost ships in the overview without unavailable active hold assessments", () => {
    const container = document.createElement("div");
    container.innerHTML = renderToStaticMarkup(
      createElement(OperationsOverview, { accounts: [recorded], dashboard }),
    );
    const lapras = container
      .querySelector('button[aria-label="Inspect Lapras, Yukine"]')!
      .closest("li")!;
    expect(lapras.textContent).toContain("Sunk");
    expect(lapras.textContent).toContain("Why: Storm.");
    expect(lapras.textContent).toContain("At sea near North sea (approximate)");
    expect(lapras.textContent).toContain("Last afloat: 12 t Grain");
    expect(lapras.textContent).not.toContain("Timber");
    expect(lapras.textContent).not.toContain("Hold assessment unavailable");
    const captured = container
      .querySelector('button[aria-label="Inspect Charmander, Yukine"]')!
      .closest("li")!;
    expect(captured.textContent).toContain("Captured");
    const missing = container
      .querySelector('button[aria-label="Inspect Squirtle, Yukine"]')!
      .closest("li")!;
    expect(missing.textContent).toContain("Missing");
    expect(missing.textContent).toContain("Last afloat cargo not recorded");
    expect(missing.textContent).not.toContain("Why:");
  });

  it("does not include terminal history rows in current decision hold assessments", () => {
    const withHolds: Account = {
      ...recorded,
      snapshot: {
        ...recorded.snapshot,
        _engine: {
          holds: [
            { ship_id: loss.ship_id, reason: "Old hold for a lost hull" },
            { ship_id: "unobserved", reason: "Awaiting ship data" },
          ],
        },
      },
    };
    const container = document.createElement("div");
    container.innerHTML = renderToStaticMarkup(
      createElement(DecisionPanel, {
        account: withHolds,
        decisions: [],
        ships: fleet(withHolds),
      }),
    );
    const holds = container.querySelector(
      'dl[aria-label="Ship-scoped automation holds"]',
    )!;
    expect(
      [...holds.querySelectorAll("dt")].map((node) => node.textContent),
    ).toEqual(["Bulbasaur", "unobserved"]);
    expect(holds.textContent).not.toContain("Old hold for a lost hull");
    expect(holds.textContent).toContain("Awaiting ship data");
  });

  it("summarizes unobserved-ship holds without UUIDs or raw reason paragraphs", () => {
    const unknown = "25fbd308-908e-4b14-a5ba-81088864665a";
    const other = "48836476-3fd6-42a5-bcba-cd379cc37467";
    const reason =
      "treasury cash must cover protected stores before staging a departure; ".repeat(
        12,
      );
    const value: Account = {
      ...account,
      snapshot: {
        ships: [detail],
        _engine: {
          holds: [
            { ship_id: unknown, reason },
            { ship_id: unknown, reason: "fresh ship records required" },
            { ship_id: other, reason },
          ],
        },
      },
    };
    const container = document.createElement("div");
    container.innerHTML = renderToStaticMarkup(
      createElement(OperationsOverview, { accounts: [value], dashboard }),
    );
    const notice = [...container.querySelectorAll("strong")].find(
      (node) => node.textContent === "Ship observations unavailable",
    )!.parentElement!;
    expect(notice.textContent).toContain(
      "Last recorded holds for 2 unobserved ships, not current: Funding needed, Fresh records needed.",
    );
    expect(notice.textContent!.length).toBeLessThan(160);
    expect(container.textContent).not.toContain(unknown);
    expect(container.textContent).not.toContain(other);
    expect(container.textContent).not.toContain(reason);
  });
});

describe("compact ship inspection", () => {
  it("does not mark a fresh capture unknown when polling leads the display clock", () => {
    const value: Account = {
      ...account,
      snapshot: {
        ships: [detail],
        _meta: {
          ships: {
            complete: true,
            generation: "capture",
            updated_at: "2026-11-01T12:00:05Z",
          },
        },
      },
    };
    const receivedAt = Date.parse("2026-11-01T12:00:06Z");
    const dashboard: DashboardData = {
      accounts: [value],
      decisions: [],
      metrics: [],
      events: [],
      requests: [],
      settlements: [],
      updated_at: "2026-11-01T12:00:06Z",
    };
    const overview = document.createElement("div");
    overview.innerHTML = renderToStaticMarkup(
      createElement(OperationsOverview, {
        accounts: [value],
        dashboard,
        receivedAt,
      }),
    );
    const row = overview
      .querySelector('button[aria-label^="Inspect "]')!
      .closest("li")!;
    expect(row.textContent).toContain("Berthed in London");
    expect(row.textContent).not.toContain("Last recorded");
    expect(row.textContent).not.toContain("freshness is unknown");
    const inspector = renderToStaticMarkup(
      createElement(ShipInspector, { account: value, detail, receivedAt }),
    );
    expect(inspector).not.toContain("Last recorded: Berthed");
    expect(inspector).not.toContain("Freshness unknown");
  });

  it("keeps assessed empty holds meaningful in Decisions without adding default ship-view noise", () => {
    const observed = "2026-11-01T11:59:55Z";
    const meta = {
      complete: true,
      generation: "capture",
      updated_at: observed,
    };
    const value: Account = {
      ...account,
      snapshot: {
        ships: [detail],
        _engine: {
          generation: "capture",
          observed_at: observed,
          assessed_ship_ids: [detail.ship.id],
          holds: [],
        },
        _meta: { ships: meta, _engine: meta },
      },
    };
    const container = document.createElement("div");
    container.innerHTML = renderToStaticMarkup(
      createElement(DecisionPanel, {
        account: value,
        decisions: [],
        ships: [detail],
      }),
    );
    const assessments = container.querySelector(
      'dl[aria-label="Ship-scoped automation holds"]',
    )!;
    expect(assessments.querySelector("dt")!.textContent).toBe("Lapras");
    expect(assessments.querySelector("dd")!.textContent).toBe(
      "No current hold recorded",
    );
    expect(
      renderToStaticMarkup(
        createElement(ShipInspector, { account: value, detail }),
      ),
    ).not.toContain("No current hold recorded");
  });

  it.each([
    { history: undefined, unavailable: false, expected: "unavailable" },
    { history: [] as ShipLoss[], unavailable: false, expected: "not recorded" },
    { history: [] as ShipLoss[], unavailable: true, expected: "unavailable" },
  ])(
    "labels detailed gone-ship history $expected when history is $history",
    ({ history, unavailable, expected }) => {
      const gone = {
        ...detail,
        ship: { ...detail.ship, status: "sunk", fate: "unknown" },
      };
      const markup = renderToStaticMarkup(
        createElement(ShipInspector, {
          account: {
            ...account,
            ship_losses: history,
            ship_losses_unavailable: unavailable,
          },
          detail: gone,
        }),
      );
      expect(markup).toContain(`Detailed loss evidence ${expected}.`);
    },
  );

  it("groups gone cargo and location once, without an empty reason or added punctuation", () => {
    const value: Account = {
      ...account,
      snapshot: { ships: [] },
      ship_losses: [{ ...loss, last_port_id: "london", hazard_cause: null }],
    };
    const [gone] = fleet(value);
    const container = document.createElement("div");
    container.innerHTML = renderToStaticMarkup(
      createElement(ShipInspector, { account: value, detail: gone }),
    );
    const heading = [...container.querySelectorAll("h3")].find(
      (node) => node.textContent === "Last afloat",
    )!;
    expect(
      [...heading.closest("section")!.querySelectorAll("p")].map(
        (node) => node.textContent,
      ),
    ).toEqual(["12 t Grain", "London"]);
    expect(
      [...container.querySelectorAll("p")].some((node) =>
        node.textContent?.startsWith("Why:"),
      ),
    ).toBe(false);
    expect(container.textContent).not.toContain("..");
  });

  it("keeps the current voyage arrival with the observed action, not stores", () => {
    const arrival = "2026-11-01T13:00:00Z";
    const sailing: ShipDetail = {
      ...detail,
      ship: { ...detail.ship, status: "sailing" },
      voyage: {
        current_leg_idx: 0,
        speed_centiknots: 300,
        estimated_arrival_at: arrival,
        legs: [],
      },
    };
    const container = render(sailing);
    const identity = [...container.querySelectorAll("strong")].find(
      (node) => node.textContent === "Lapras",
    )!.parentElement!;
    const stores = [...container.querySelectorAll("h3")]
      .find((node) => node.textContent === "Stores")!
      .closest("section")!;
    expect(identity.textContent).toContain(`Est. ${dateTime(arrival)}`);
    expect(stores.textContent).not.toContain(`Est. ${dateTime(arrival)}`);
  });

  it.each([
    { manifest: null, expected: "Cargo manifest not recorded." },
    { manifest: {}, expected: "No trade cargo in the recorded manifest." },
  ])(
    "distinguishes an unknown manifest from a recorded empty one ($expected)",
    ({ manifest, expected }) => {
      const container = render({ ...detail, cargo_manifest: manifest });
      const section = [...container.querySelectorAll("h3")]
        .find((node) => node.textContent === "Cargo manifest")!
        .closest("section")!;
      expect(section.textContent).toContain(expected);
    },
  );

  it("keeps zero effective capacity and explicit empty berth timing distinct from missing data", () => {
    const container = render({
      ...detail,
      effective_hold_capacity_tons: 0,
      cargo_used_tons: 0,
      ship: {
        ...detail.ship,
        berthed_at: null,
        dock_queued_at: null,
        turnaround_unlocks_at: null,
      },
    });
    expect(container.textContent).toContain(
      "0 / 0 t of effective cargo capacity.",
    );
    expect(container.textContent).toContain("No berth recorded");
    expect(container.textContent).toContain("No queue recorded");
    expect(container.textContent).toContain("No lock recorded");
    expect(container.textContent).not.toContain(
      "Cargo capacity use not recorded.",
    );
  });

  it("does not invent a legacy valuation for a hold decision with null details", () => {
    const markup = renderToStaticMarkup(
      createElement(ShipInspector, {
        account,
        detail,
        decisions: [
          {
            id: 1,
            account_id: account.id,
            ship_id: detail.ship.id,
            created_at: "2026-11-01T12:00:00Z",
            kind: "hold",
            reason: "",
            details: null,
          },
        ],
      }),
    );
    expect(markup).toContain("Economics not recorded.");
    expect(markup).toContain("Reason not recorded.");
    expect(markup).not.toContain("Legacy recorded model");
    expect(markup).not.toContain("Recorded before destination-based valuation");
  });

  it("shows the full manifest and passenger destinations before the only disclosure", () => {
    const loaded = {
      ...detail,
      ship: { ...detail.ship, passenger_hold_tons: 7 },
      cargo_manifest: { grain: 12, timber: 8, wool: 5 },
    };
    const observed = "2026-11-01T11:59:55Z";
    const meta = {
      complete: true,
      generation: "capture",
      updated_at: observed,
    };
    const value: Account = {
      ...account,
      snapshot: {
        ships: [loaded],
        catalog: {
          ports: [],
          sea_zones: [],
          ship_classes: [],
          commodities: [
            { id: "grain", name: "Barley grain" },
            { id: "timber", name: "Oak timber" },
            { id: "wool", name: "Fine wool" },
          ],
        },
        passenger_aboard: {
          [loaded.ship.id]: [
            {
              id: "group",
              kind: "travellers",
              size: 7,
              origin_port_id: "london",
              dest_port_id: "hamburg",
              hold_tons: 7,
              fare_per_head_pence: 100,
              full_fare_pence: 700,
            },
          ],
        },
        _meta: { ships: meta, [`passenger_aboard/${loaded.ship.id}`]: meta },
      },
    };
    const container = document.createElement("div");
    container.innerHTML = renderToStaticMarkup(
      createElement(ShipInspector, { account: value, detail: loaded }),
    );
    const disclosure = container.querySelector("details")!;
    expect(container.querySelectorAll("details")).toHaveLength(1);
    expect(disclosure.open).toBe(false);
    expect(disclosure.querySelector("summary")!.textContent).toBe(
      "More details",
    );
    const manifest = container.querySelector("ul")!;
    expect(manifest.closest("details")).toBeNull();
    expect(
      [...manifest.querySelectorAll("li")].map((node) => node.textContent),
    ).toEqual(["Barley grain12 t", "Oak timber8 t", "Fine wool5 t"]);
    const passengers = container.querySelector(
      '[aria-label="Passengers by destination"]',
    )!;
    expect(passengers.closest("details")).toBeNull();
    expect(passengers.textContent).toContain("To Hamburg7 passengers");
    expect(disclosure.textContent).toContain("Agreed fare per person");
  });

  it("preserves an opened native disclosure when recorded ship data refreshes", async () => {
    vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
    const container = document.createElement("div");
    document.body.append(container);
    const root = createRoot(container);
    try {
      await act(async () =>
        root.render(createElement(ShipInspector, { account, detail })),
      );
      const disclosure = container.querySelector("details")!;
      await act(async () => disclosure.querySelector("summary")!.click());
      expect(disclosure.open).toBe(true);
      await act(async () =>
        root.render(
          createElement(ShipInspector, {
            account,
            detail: { ...detail, cargo_manifest: { grain: 10 } },
          }),
        ),
      );
      expect(container.querySelector("details")).toBe(disclosure);
      expect(disclosure.open).toBe(true);
      expect(container.querySelector("ul")!.textContent).toContain("Grain10 t");
    } finally {
      await act(async () => root.unmount());
      container.remove();
      vi.unstubAllGlobals();
    }
  });
});

describe("Overview ship dialog", () => {
  const observed = "2026-11-01T11:59:55Z";
  const meta = { complete: true, generation: "capture", updated_at: observed };
  const loaded: ShipDetail = {
    ...detail,
    ship: { ...detail.ship, passenger_hold_tons: 0 },
    cargo_manifest: { grain: 12, timber: 4 },
  };
  const value: Account = {
    ...account,
    snapshot: {
      ships: [loaded],
      catalog: {
        ports: [{ id: "london", name: "London" }],
        sea_zones: [],
        commodities: [
          { id: "grain", name: "Grain" },
          { id: "timber", name: "Timber" },
        ],
        ship_classes: [{ id: "hoy", name: "Hoy", crew_required: 10 }],
      },
      _meta: { ships: meta },
    },
    missions: [
      {
        ship_id: detail.ship.id,
        phase: "purchase",
        updated_at: observed,
        plan: {
          kind: "spot",
          origin: "london",
          destination: "hamburg",
          commodity: "grain",
          quantity: 12,
          economics: {},
        },
      },
    ],
  };
  const dashboard: DashboardData = {
    updated_at: observed,
    accounts: [value],
    decisions: [],
    events: [],
    metrics: [],
    requests: [],
    settlements: [],
  };
  let root: Root;
  let container: HTMLDivElement;
  beforeEach(() => {
    vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
    vi.stubGlobal("matchMedia", () => ({
      matches: false,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    }));
    useUI.setState({
      selectedShipID: detail.ship.id,
      selectedShipAccountID: account.id,
    });
    container = document.createElement("div");
    document.body.append(container);
    root = createRoot(container);
  });
  afterEach(async () => {
    await act(async () => root.unmount());
    container.remove();
    useUI.setState({ selectedShipID: "", selectedShipAccountID: "" });
    vi.unstubAllGlobals();
  });

  it("shows and clears a persisted selection without opening the inspector", async () => {
    await act(async () =>
      root.render(
        createElement(FleetPanel, {
          accounts: [value],
          dashboard,
          compact: true,
        }),
      ),
    );
    const selection = container.querySelector(
      'section[aria-label="Ship selection"]',
    )!;
    expect(selection.textContent).toContain("Selected ship: Lapras, Yukine.");
    expect(selection.textContent).toContain("Inspect selected ship");
    expect(document.querySelector('[role="dialog"]')).toBeNull();
    await act(async () =>
      [...selection.querySelectorAll<HTMLButtonElement>("button")]
        .find((button) => button.textContent === "Clear ship selection")!
        .click(),
    );
    expect(useUI.getState().selectedShipID).toBe("");
    expect(
      container.querySelector('section[aria-label="Ship selection"]'),
    ).toBeNull();
  });

  it.each([
    {
      selectedAccountID: account.id,
      message: "Ship unavailable",
      inspect: true,
    },
    {
      selectedAccountID: "another-account",
      message: "outside this account scope",
      inspect: false,
    },
  ])(
    "keeps a persisted $message selection clearable without exposing its ID",
    async ({ selectedAccountID, message, inspect }) => {
      const missingID = "7ecbba47-de05-46e5-a2fb-605551f471ab";
      useUI.setState({
        selectedShipID: missingID,
        selectedShipAccountID: selectedAccountID,
      });
      await act(async () =>
        root.render(
          createElement(FleetPanel, {
            accounts: [value],
            dashboard,
            compact: true,
          }),
        ),
      );
      const selection = container.querySelector(
        'section[aria-label="Ship selection"]',
      )!;
      expect(selection.textContent).toContain(message);
      expect(selection.textContent).toContain("Clear ship selection");
      expect(
        [...selection.querySelectorAll("button")].some(
          (button) => button.textContent === "Inspect selected ship",
        ),
      ).toBe(inspect);
      expect(container.textContent).not.toContain(missingID);
      await act(async () =>
        [...selection.querySelectorAll<HTMLButtonElement>("button")]
          .find((button) => button.textContent === "Clear ship selection")!
          .click(),
      );
      expect(useUI.getState().selectedShipID).toBe("");
    },
  );

  it("computes one compact summary pass and refreshes its time-sensitive status", async () => {
    const summarize = vi.spyOn(shipSummaries, "shipSummary");
    const accounts = [value];
    const show = (receivedAt = 0) =>
      act(async () =>
        root.render(
          createElement(FleetPanel, {
            accounts,
            dashboard,
            compact: true,
            receivedAt,
          }),
        ),
      );
    try {
      await show();
      expect(summarize).toHaveBeenCalledTimes(1);
      await show();
      expect(summarize).toHaveBeenCalledTimes(1);
      await show(Date.parse("2026-11-01T12:02:10Z"));
      expect(summarize).toHaveBeenCalledTimes(2);
      const row = container
        .querySelector('button[aria-label="Inspect Lapras, Yukine"]')!
        .closest("li")!;
      expect(row.textContent).toContain("Ship records are stale.");
    } finally {
      summarize.mockRestore();
    }
  });

  it("keeps a delayed observation's mission in the default ship summary", () => {
    const delayed: Account = {
      ...value,
      snapshot: {
        ...value.snapshot,
        _meta: {
          ships: { ...meta, updated_at: "2026-11-01T11:59:15Z" },
        },
      },
    };
    const container = document.createElement("div");
    container.innerHTML = renderToStaticMarkup(
      createElement(OperationsOverview, { accounts: [delayed], dashboard }),
    );
    const row = container
      .querySelector('button[aria-label="Inspect Lapras, Yukine"]')!
      .closest("li")!;
    expect(row.textContent).toContain("Why: Trade 12 t Grain to Hamburg.");
    expect(row.textContent).not.toContain("Ship records are delayed");
    expect(row.textContent).not.toContain("Last recorded: Berthed");
  });

  it("shows a blocker before a benign voyage wait, even with an active mission", () => {
    const sailing: ShipDetail = {
      ...loaded,
      ship: { ...loaded.ship, status: "sailing" },
    };
    const blocked: Account = {
      ...value,
      snapshot: {
        ...value.snapshot,
        ships: [sailing],
        _engine: {
          generation: "capture",
          observed_at: observed,
          assessed_ship_ids: [sailing.ship.id],
          holds: [
            {
              ship_id: sailing.ship.id,
              reason: "waiting for authoritative arrival",
            },
            {
              ship_id: sailing.ship.id,
              reason: "insufficient cash for protected stores",
            },
          ],
        },
        _meta: { ships: meta, _engine: meta },
      },
    };
    const container = document.createElement("div");
    container.innerHTML = renderToStaticMarkup(
      createElement(OperationsOverview, { accounts: [blocked], dashboard }),
    );
    const row = container
      .querySelector('button[aria-label="Inspect Lapras, Yukine"]')!
      .closest("li")!;
    expect(row.textContent).toContain("Why: Funding needed.");
    expect(row.textContent).not.toContain("Why: Waiting for arrival.");
  });

  it("captures a new external opener instead of retaining a prior dialog trigger", async () => {
    await act(async () =>
      root.render(
        createElement(FleetPanel, {
          accounts: [value],
          dashboard,
          compact: true,
        }),
      ),
    );
    const first = container.querySelector<HTMLButtonElement>(
      'button[aria-label="Inspect Lapras, Yukine"]',
    )!;
    await act(async () => {
      first.focus();
      first.click();
    });
    await act(async () =>
      document
        .querySelector<HTMLButtonElement>(
          '[role="dialog"] button[aria-label="Close ship inspector"]',
        )!
        .click(),
    );
    await vi.waitFor(() => expect(document.activeElement).toBe(first));
    await act(async () =>
      root.render(createElement(FleetPanel, { accounts: [value], dashboard })),
    );
    const external = document.createElement("button");
    external.textContent = "Select ship from another control";
    document.body.append(external);
    try {
      await act(async () => {
        external.focus();
        useUI.getState().selectShip(loaded.ship.id, value.id);
      });
      expect(document.querySelector('[role="dialog"]')).not.toBeNull();
      await act(async () =>
        document
          .querySelector<HTMLButtonElement>(
            '[role="dialog"] button[aria-label="Close ship inspector"]',
          )!
          .click(),
      );
      await vi.waitFor(() => expect(document.activeElement).toBe(external));
    } finally {
      external.remove();
    }
  });
  it.each([
    ['button[aria-label="Inspect Lapras, Yukine"]', "Escape"],
    ['button[aria-label="More details about Lapras"]', "Close"],
  ])(
    "only opens from its own trigger and returns focus (%s)",
    async (selector, close) => {
      await act(async () =>
        root.render(
          createElement(FleetPanel, {
            accounts: [value],
            dashboard,
            compact: true,
          }),
        ),
      );
      expect(document.querySelector('[role="dialog"]')).toBeNull();
      const trigger = container.querySelector<HTMLButtonElement>(selector)!;
      const row = trigger.closest("li")!;
      expect(row.textContent).toContain("Lapras");
      expect(row.textContent).toContain("Berthed in London");
      expect(row.textContent).toContain("Cargo 12 t Grain, 4 t Timber");
      expect(row.textContent).toContain("Crew required 10");
      expect(row.textContent).toContain("Passengers 0");
      expect(row.textContent).toContain("Why: Trade 12 t Grain to Hamburg.");
      expect(
        row.querySelector(".sprite-image")!.getAttribute("aria-hidden"),
      ).toBe("true");
      expect(trigger.hasAttribute("aria-pressed")).toBe(false);
      expect(trigger.getAttribute("aria-expanded")).toBe("false");
      await act(async () => trigger.click());
      const dialog = document.querySelector('[role="dialog"]')!;
      expect(dialog).not.toBeNull();
      expect(dialog.querySelectorAll("details")).toHaveLength(1);
      await act(async () => {
        if (close === "Escape") {
          document.dispatchEvent(
            new KeyboardEvent("keydown", { key: "Escape", bubbles: true }),
          );
        } else {
          dialog
            .querySelector<HTMLButtonElement>(
              'button[aria-label="Close ship inspector"]',
            )!
            .click();
        }
      });
      expect(document.querySelector('[role="dialog"]')).toBeNull();
      await vi.waitFor(() => expect(document.activeElement).toBe(trigger));
    },
  );
  it.each(["account leaves scope", "Fleet round trip"])(
    "does not reopen after %s",
    async (change) => {
      const render = async (accounts: Account[], compact: boolean) =>
        act(async () =>
          root.render(
            createElement(FleetPanel, { accounts, dashboard, compact }),
          ),
        );
      await render([value], true);
      await act(async () =>
        container
          .querySelector<HTMLButtonElement>(
            'button[aria-label="Inspect Lapras, Yukine"]',
          )!
          .click(),
      );
      expect(document.querySelector('[role="dialog"]')).not.toBeNull();
      await render(
        change === "account leaves scope" ? [] : [value],
        change === "account leaves scope",
      );
      await render([value], true);
      expect(document.querySelector('[role="dialog"]')).toBeNull();
      expect(
        container
          .querySelector('button[aria-label="Inspect Lapras, Yukine"]')!
          .getAttribute("aria-expanded"),
      ).toBe("false");
      const selection = container.querySelector(
        'section[aria-label="Ship selection"]',
      )!;
      expect(selection.textContent).toContain("Selected ship: Lapras, Yukine.");
      expect(selection.textContent).toContain("Clear ship selection");
    },
  );
});
