// @vitest-environment jsdom

import { act, createElement } from "react";
import { createRoot, type Root } from "react-dom/client";
import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fleet } from "@/lib/data";
import { dateTime, money } from "@/lib/format";
import type { Account, DashboardData, ShipDetail, ShipLoss } from "@/lib/types";
import { useUI } from "@/lib/ui-store";
import { DecisionPanel } from "./decisions";
import { FleetPanel, ShipInspector } from "./fleet";
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
      ["broken_up", "Broken up", "exit"],
      ["royal_navy", "Commissioned to the Royal Navy", "exit"],
      ["missing", "Missing from fleet list", "absence"],
    ];
    const exits: Account = {
      ...account,
      snapshot: { ships: [] },
      ship_losses: fates.map(([fate]) => ({
        ...loss,
        ship_id: fate,
        ship_name: fate,
        fate,
      })),
    };
    const container = document.createElement("div");
    container.innerHTML = renderToStaticMarkup(
      createElement(FleetPanel, { accounts: [exits], dashboard }),
    );
    const rows = [...container.querySelectorAll("tbody tr")];
    expect(rows).toHaveLength(3);
    for (const [fate, label, kind] of fates) {
      const row = rows.find((item) =>
        item.querySelector("button")?.textContent?.includes(fate),
      )!;
      expect(row.textContent).toContain(label);
      expect(row.textContent).toContain(`Retained ${kind} record`);
      expect(row.textContent).toContain(`See ${kind} record`);
      expect(row.textContent?.toLowerCase()).not.toContain("loss");
      expect(row.querySelector(".lucide-skull")).toBeNull();
    }
    container.innerHTML = renderToStaticMarkup(
      createElement(OperationsOverview, { accounts: [exits], dashboard }),
    );
    expect(container.querySelectorAll("li")).toHaveLength(3);
    for (const [, label, kind] of fates) {
      const row = [...container.querySelectorAll("li")].find((item) =>
        item.textContent?.includes(label),
      )!;
      expect(row.textContent).toContain(`Retained ${kind} record`);
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
    expect(lapras.textContent).toContain("Retained loss record");
    expect(lapras.textContent).toContain("At sea near North sea (approximate)");
    expect(lapras.textContent).toContain("Last afloat: 8 sea days");
    expect(lapras.textContent).toContain("See loss record");
    const legacy = [...container.querySelectorAll("tbody tr")].find((row) =>
      row.querySelector("button")?.textContent?.includes("Gastly"),
    )!;
    expect(legacy.querySelectorAll("td")[2].textContent).toContain("Sunk");
    expect(legacy.querySelectorAll("td")[5].textContent).toBe("Not recorded");
    expect(legacy.textContent).not.toContain("See loss record");
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
        (row) =>
          row.querySelectorAll("td")[2].querySelector(".badge")?.textContent,
      ),
    ).toEqual([
      "Berthed",
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
    expect(lapras.textContent).toContain("Retained loss record");
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
    expect(missing.textContent).toContain("Retained absence record");
    expect(missing.textContent).not.toContain("Retained loss record");
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
});
