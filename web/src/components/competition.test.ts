import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type * as Data from "@/lib/data";
import type { CompetitionSummary } from "@/lib/types";
import { CompetitionView } from "./competition";

const state = vi.hoisted(() => ({
  data: undefined as CompetitionSummary | undefined,
  isError: false,
  error: new Error("Service unavailable"),
}));

vi.mock("@/lib/data", async (original) => ({
  ...(await original<typeof Data>()),
  useCompetition: () => state,
}));
vi.mock("@/app/providers", () => ({
  useNow: () => Date.parse("2026-10-26T12:00:00Z"),
}));

function fixture(): CompetitionSummary {
  const activity = {
    foreign_companies: 0,
    foreign_ships: 0,
    sightings: 0,
    last_seen: null,
    coverage: "partial" as const,
    top_competitors: [],
  };
  return {
    account_id: "one",
    generated_at: "2026-10-26T12:00:00Z",
    from: "2026-10-25T12:00:00Z",
    to: "2026-10-26T12:00:00Z",
    identity_basis: "company_and_ship_name",
    own_companies_complete: true,
    events_truncated: false,
    regions: [
      {
        ...activity,
        id: "north_sea",
        observed_ports: 1,
        ports: [
          {
            ...activity,
            id: "a",
            name: "Port A",
            observed_at: "2026-10-26T11:59:00Z",
            quay_observed_at: "2026-10-26T11:59:00Z",
            source_stale: false,
            player_volume_tons: 0,
            player_turnover_pence: null,
            occupied_berths: null,
            effective_berths: null,
            queued_ships: null,
            ships: [],
          },
        ],
      },
      {
        ...activity,
        id: "baltic",
        coverage: "not_observed",
        foreign_companies: null,
        foreign_ships: null,
        observed_ports: 0,
        ports: [],
      },
    ],
  };
}

function render() {
  return renderToStaticMarkup(
    createElement(CompetitionView, { accountID: "one" }),
  );
}

describe("competition disclosure", () => {
  beforeEach(() => {
    state.data = fixture();
    state.isError = false;
  });

  it("distinguishes observed zero from unobserved traffic with native drill-in", () => {
    const html = render();
    expect(html).toContain("0 companies, 0");
    expect(html).toContain("Not observed: Baltic.");
    expect(html).not.toContain("No quay coverage");
    expect(html).toContain("24 real hours");
    expect(html).toContain("not live ship");
    expect(html).toContain("<details");
    expect(html).toContain("<summary");
    expect(html).toContain("Port A");
    expect(html).toContain("Not recorded");
    expect(html).toContain("window unspecified");
    expect(html.match(/name-based/g)).toHaveLength(1);
  });

  it("lists observed regions by ships before the collapsed unobserved line", () => {
    const quiet = state.data!.regions[0];
    state.data!.regions.push({
      ...quiet,
      id: "channel",
      foreign_companies: 6,
      foreign_ships: 48,
    });
    const html = render();
    const channel = html.indexOf(">Channel<");
    const northSea = html.indexOf(">North sea<");
    const unobserved = html.indexOf("Not observed: Baltic.");
    expect(channel).toBeGreaterThan(-1);
    expect(channel).toBeLessThan(northSea);
    expect(northSea).toBeLessThan(unobserved);
  });

  it("shows at most five companies ranked by ships with their latest sighting", () => {
    const port = state.data!.regions[0].ports[0];
    port.ships = [
      { name: "Quiet Quay", ships: 1 },
      { name: "Bay Trading", ships: 2 },
      { name: "Amber Company", ships: 2 },
      { name: "Cargo Fleet", ships: 3 },
      { name: "Delta", ships: 1 },
      { name: "Echo", ships: 1 },
      { name: "Foxtrot", ships: 1 },
    ].flatMap((company) =>
      Array.from({ length: company.ships }, (_, index) => ({
        company_name: company.name,
        ship_name: `Private ship detail ${company.name} ${index}`,
        ship_class: "hoy",
        last_seen:
          index === 0 ? "2026-10-26T11:00:00Z" : "2026-10-26T11:50:00Z",
        direction: "sailed",
        reported_other_port_id: "hamburg",
      })),
    );
    const html = render();
    const rows = [
      ...html.matchAll(/<tr><th scope="row">([^<]+)<\/th><td>([^<]+)<\/td>/g),
    ].map((row) => [row[1], row[2]]);
    expect(rows).toEqual([
      ["Cargo Fleet", "3"],
      ["Amber Company", "2"],
      ["Bay Trading", "2"],
      ["Delta", "1"],
      ["Echo", "1"],
    ]);
    expect(html).toContain("Companies observed at Port A");
    expect(html).toContain("Ships seen");
    expect(html).toContain("Last seen");
    expect(html).toContain("2026-10-26T11:50:00Z");
    expect(html).toContain("10m ago");
    expect(html).toContain("and 2 more");
    expect(html).not.toContain("Private ship detail");
    expect(html).not.toContain("Reported origin/destination");
  });

  it("discloses bounded evidence and missing ownership exclusions", () => {
    state.data!.events_truncated = true;
    state.data!.own_companies_complete = false;
    const html = render();
    expect(html).toContain("The event limit was reached");
    expect(html).toContain("Foreign counts");
    expect(html).toContain("remain unknown");
  });

  it("keeps retained data visible on read failure and does not fake an empty success", () => {
    state.isError = true;
    const retained = render();
    expect(retained).toContain("Service unavailable");
    expect(retained).toContain("Port A");
    state.data = undefined;
    const unavailable = render();
    expect(unavailable).toContain("Competition could not be loaded");
    expect(unavailable).not.toContain("0 companies");
    expect(unavailable).not.toContain("No recorded port catalog");
  });
});
