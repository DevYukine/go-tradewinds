// @vitest-environment jsdom

import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";
import { dateTime } from "@/lib/format";
import type { Account, ShipDetail } from "@/lib/types";
import { ShipInspector } from "./fleet";

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

function render(former_names?: ShipDetail["former_names"]) {
  const container = document.createElement("div");
  container.innerHTML = renderToStaticMarkup(
    createElement(ShipInspector, {
      account,
      detail: { ...detail, former_names },
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
    const container = render(formerNames);
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
      const container = render(names);
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
    const container = render([{ name, renamed_at: "2026-11-01T10:30:00Z" }]);
    const label = [...container.querySelectorAll("dt")].find(
      (element) => element.textContent === name,
    );
    expect(label).toBeDefined();
    expect(label!.querySelector("img")).toBeNull();
  });
});
