// @vitest-environment jsdom

import { act, createElement } from "react";
import { createRoot, type Root } from "react-dom/client";
import type * as ReactLeaflet from "react-leaflet";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { shipOperation } from "@/lib/operations";
import type { Account, ShipDetail, ShipLoss } from "@/lib/types";
import type { ChartVessel } from "../fleet-map";
import FleetChart from "./fleet-chart";

// Keep the real map and imperative vessel markers. Static coastline, grid and
// port layers need an SVG layout engine that jsdom does not provide.
vi.mock("react-leaflet", async (original) => ({
  ...(await original<typeof ReactLeaflet>()),
  GeoJSON: () => null,
  Polyline: () => null,
  Marker: () => null,
  CircleMarker: () => null,
  Tooltip: () => null,
  Popup: () => null,
  ScaleControl: () => null,
  AttributionControl: () => null,
}));

const observedAt = "2026-11-03T12:00:00Z";
const now = Date.parse(observedAt);
const afloat: ShipDetail = {
  ship: {
    id: "lapras",
    company_id: "company-a",
    name: "Lapras",
    ship_class: "hoy",
    status: "berthed",
    current_port_id: "london",
    current_leg_idx: null,
    cannons_count: null,
    stores_provisions_units: null,
    updated_at: observedAt,
  },
  hold_capacity_tons: null,
  effective_hold_capacity_tons: null,
  cargo_used_tons: null,
  cargo_manifest: null,
  stores: null,
  route_legs: [],
};
const loss: ShipLoss = {
  account_id: "account-a",
  ship_id: "lapras",
  ship_name: "Lapras",
  ship_class: "hoy",
  status: "sunk",
  fate: "lost_at_sea",
  observed_at: observedAt,
  last_observed_at: "2026-11-03T11:30:00Z",
  recovered_at: null,
  last_port_id: "london",
  last_sea_zone_id: "north_sea",
  voyage_origin_id: "london",
  voyage_destination_id: "hamburg",
  cargo_manifest: null,
  stores: null,
  hazard_cause: null,
  insurance_claim_paid_pence: null,
};

function retained(fate: string): ShipDetail {
  return {
    ...afloat,
    ship: { ...afloat.ship, status: "sunk", fate },
    loss: { ...loss, fate },
  };
}

function vessel(detail: ShipDetail): ChartVessel {
  const account: Account = {
    id: "account-a",
    label: "Yukine",
    company_id: "company-a",
    enabled: true,
    created_at: observedAt,
    snapshot: {
      ships: [detail],
      _meta: { ships: { updated_at: observedAt, complete: true } },
    },
    ship_losses: detail.loss ? [detail.loss] : [],
  };
  return {
    key: "account-a:lapras",
    account,
    company: "Pokemon Pirates",
    detail,
    meta: { updated_at: observedAt, complete: true },
    clockMeta: {},
    operation: shipOperation(account, detail, now),
    segments: [],
  };
}

describe("retained ship markers", () => {
  let root: Root;
  let container: HTMLDivElement;
  const onSelect = vi.fn();
  const onSelectPort = vi.fn();
  const onReady = vi.fn();
  const onPositionNote = vi.fn();

  beforeEach(() => {
    vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
    vi.stubGlobal(
      "requestAnimationFrame",
      vi.fn(() => 1),
    );
    vi.stubGlobal("cancelAnimationFrame", vi.fn());
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(document, "hidden", "get").mockReturnValue(false);
    container = document.createElement("div");
    document.body.append(container);
    root = createRoot(container);
    onPositionNote.mockClear();
  });

  afterEach(async () => {
    await act(async () => root.unmount());
    container.remove();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  async function render(detail: ShipDetail) {
    await act(async () => {
      root.render(
        createElement(FleetChart, {
          vessels: [vessel(detail)],
          snapshotUTC: observedAt,
          snapshotReceivedAt: now,
          inspectorOpen: false,
          selectedPortID: "",
          selectedKey: "account-a:lapras",
          layers: { routes: true, labels: true, estimates: true },
          rosterOpen: false,
          onSelect,
          onSelectPort,
          onReady,
          onPositionNote,
        }),
      );
    });
  }

  function marker() {
    return container.querySelector<HTMLElement>('[aria-label^="Lapras."]');
  }

  it.each([
    ["broken_up", "Broken up"],
    ["royal_navy", "Commissioned to the Royal Navy"],
    ["missing", "Missing from fleet list"],
    ["seized_smuggling", "Seized for smuggling"],
    ["seized_bailiff", "Seized by the bailiff"],
    ["seized_bankruptcy", "Seized after bankruptcy"],
  ])(
    "replaces a stale wreck glyph with a non-wreck marker for %s",
    async (fate, label) => {
      await render(afloat);
      const liveMarker = marker()!;
      const liveGlyph = liveMarker.querySelector("svg")!.outerHTML;
      await render(retained("lost_at_sea"));
      expect(marker()).toBe(liveMarker);
      expect(marker()!.querySelector("svg")!.outerHTML).not.toBe(liveGlyph);
      await render(retained(fate));
      expect(marker()).toBe(liveMarker);
      expect(marker()!.querySelector("svg")!.outerHTML).toBe(liveGlyph);
      expect(marker()!.getAttribute("aria-label")).toContain(label);
      expect(marker()!.getAttribute("aria-label")?.toLowerCase()).not.toContain(
        "loss location",
      );
    },
  );

  it.each(["lost_at_sea", "captured", "unknown", undefined, null, ""])(
    "uses a wreck only for an observed true-loss port (fate=%s)",
    async (fate) => {
      await render(afloat);
      const liveGlyph = marker()!.querySelector("svg")!.outerHTML;
      await render({
        ...afloat,
        ship: { ...afloat.ship, status: "sunk", fate },
      });
      expect(marker()!.querySelector("svg")!.outerHTML).not.toBe(liveGlyph);
      expect(marker()!.getAttribute("aria-label")).toContain(
        fate === "captured" ? "Captured" : "Sunk",
      );
    },
  );

  it.each([
    "lost_at_sea",
    "captured",
    "broken_up",
    "royal_navy",
    "missing",
    "seized_smuggling",
  ])(
    "removes the old marker when %s was last observed at sea",
    async (fate) => {
      await render(afloat);
      expect(marker()).not.toBeNull();
      const detail = retained(fate);
      await render({
        ...detail,
        loss: { ...detail.loss!, last_port_id: null },
      });
      expect(marker()).toBeNull();
      expect(onPositionNote.mock.calls.at(-1)?.[1]).toContain(
        "At-sea observations remain unplaced",
      );
      await render(detail);
      expect(marker()).not.toBeNull();
      expect(marker()!.getAttribute("aria-label")).toContain(
        "Last observed port",
      );
    },
  );

  it("does not plot a legacy sunk hull from a retained route or old marker position", async () => {
    await render(afloat);
    expect(marker()).not.toBeNull();
    await render({
      ...afloat,
      ship: { ...afloat.ship, status: "sunk", current_port_id: null },
      route_legs: [
        {
          from_node: "london",
          to_node: "hamburg",
          distance_nm: 400,
          edge_class: "open_sea",
          is_sound_toll: false,
        },
      ],
    });
    expect(marker()).toBeNull();
  });
});
