"use client";

// Leaflet owns imperative DOM; remount it instead of reusing a removed map in HMR.
// @refresh reset

import type { GeoJsonObject } from "geojson";
import {
  circleMarker,
  divIcon,
  latLngBounds,
  marker,
  point,
  polyline,
  type CircleMarker as LeafletCircle,
  type LatLngTuple,
  type Map as LeafletMap,
  type Marker as LeafletMarker,
  type Polyline as LeafletLine,
} from "leaflet";
import { memo, useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  AttributionControl,
  CircleMarker,
  GeoJSON,
  MapContainer,
  Marker,
  Polyline,
  Popup,
  ScaleControl,
  Tooltip,
  useMap,
  useMapEvents,
} from "react-leaflet";
import {
  chartBlockingHolds,
  chartETA,
  chartLossPort,
  chartPortActivity,
  chartPosition,
  chartUnavailableReason,
  splitChartRoute,
  voyageETA,
  type ChartSegment,
} from "@/lib/chart";
import { dateTime, humanize, percent } from "@/lib/format";
import {
  PORT_COORDINATES,
  nodeCoordinate,
  travelBearing,
  type ChartPosition,
  type Coordinate,
} from "@/lib/geography";
import { shipIsLoss, shipStatusLabel } from "@/lib/operations";
import type { Catalog, Port } from "@/lib/types";
import type { ChartControls, ChartLayers, ChartVessel } from "../fleet-map";
import styles from "./chart.module.css";
import land from "./ne_50m_land.json";
import "leaflet/dist/leaflet.css";

const EUROPE: [LatLngTuple, LatLngTuple] = [
  [34, -16],
  [62, 28],
];
const GRATICULE: LatLngTuple[][] = [
  ...Array.from({ length: 15 }, (_, index): LatLngTuple[] => [
    [-70 + index * 10, -180],
    [-70 + index * 10, 180],
  ]),
  ...Array.from({ length: 37 }, (_, index): LatLngTuple[] => [
    [-80, -180 + index * 10],
    [85, -180 + index * 10],
  ]),
];
const LAND_STYLE = { className: styles.land, weight: 0.8 };
const GRID_STYLE = {
  color: "var(--chart-grid)",
  weight: 0.6,
  opacity: 0.65,
  interactive: false,
};
const EMPTY_PORTS: Port[] = [];
const SEA_NAMES: { name: string; coordinate: LatLngTuple }[] = [
  { name: "North Atlantic Ocean", coordinate: [35, -43] },
  { name: "Caribbean Sea", coordinate: [15, -73] },
  { name: "South Atlantic Ocean", coordinate: [-15, -21] },
  { name: "Indian Ocean", coordinate: [-16, 72] },
];
const SEA_ICONS = SEA_NAMES.map(({ name }) =>
  divIcon({
    className: styles.seaName,
    html: name,
    iconSize: [220, 40],
    iconAnchor: [110, 20],
  }),
);
const SHIP_GLYPH =
  '<svg viewBox="0 0 28 38" aria-hidden="true"><path class="hull" d="M14 2C9 7 6 17 7 25l3 9h8l3-9c1-8-2-18-7-23Z"/><path class="deck" d="M14 9v20m-5-9h10m-9 6h8"/></svg>';
const WRECK_GLYPH =
  '<svg viewBox="0 0 28 38" aria-hidden="true"><path class="hull" d="m5 24 7 4 11-7-5 12H8Z"/><path class="deck" d="m9 8 12 12M21 8 9 20M14 20v9"/></svg>';

interface TrackedVessel {
  marker: LeafletMarker;
  label: HTMLElement;
  tooltip: HTMLElement;
  position?: ChartPosition;
  segments?: ChartSegment[];
  splitLatitude?: number;
  splitLongitude?: number;
  splitLeg?: number | null;
  glyph: HTMLElement;
  wreck: boolean;
  gone: boolean;
  leader: LeafletLine;
  labelLeader: LeafletLine;
  arrivalLeader: LeafletLine;
  wake: LeafletLine;
  remaining: LeafletLine;
  destination: LeafletCircle;
  arrival: HTMLElement;
}

interface ChartFrame {
  vessels: ChartVessel[];
  layers: ChartLayers;
  selectedKey: string;
  snapshotUTC?: string;
  serverUTC: number;
  monotonicAt: number;
  wallAt: number;
  inspectorOpen: boolean;
  rosterOpen: boolean;
}

function routePoints(segments: ChartSegment[]): LatLngTuple[][] {
  return segments.map(({ from, to }) => [
    [from.latitude, from.longitude],
    [to.latitude, to.longitude],
  ]);
}

function coordinateTuple(coordinate: Coordinate): LatLngTuple {
  return [coordinate.latitude, coordinate.longitude];
}

function layoutVesselLabels(
  map: LeafletMap,
  registry: Map<string, TrackedVessel>,
  selectedKey: string,
) {
  const size = map.getSize();
  const occupied: {
    left: number;
    top: number;
    right: number;
    bottom: number;
  }[] = [];
  const root = map.getContainer().closest("main");
  for (const element of root?.querySelectorAll(
    ":scope > header, :scope > [class*='recordingStatus'], :scope > footer, :scope > nav, :scope > [role='group'], :scope > div > aside, [class*='north'], .leaflet-control, [class*='coordinates'] > span",
  ) ?? []) {
    const box = element.getBoundingClientRect();
    if (box.width && box.height) occupied.push(box);
  }
  for (const element of map
    .getContainer()
    .querySelectorAll(`.${styles.portName}`)) {
    occupied.push(element.getBoundingClientRect());
  }
  const entries = [...registry.entries()].sort(
    ([left], [right]) =>
      Number(right === selectedKey) - Number(left === selectedKey) ||
      left.localeCompare(right),
  );
  for (const [, entry] of entries) {
    if (!map.hasLayer(entry.marker)) continue;
    const anchor = map.latLngToContainerPoint(entry.marker.getLatLng());
    occupied.push({
      left: anchor.x - 13,
      top: anchor.y - 17,
      right: anchor.x + 13,
      bottom: anchor.y + 17,
    });
  }
  function place(
    anchor: { x: number; y: number },
    width: number,
    height: number,
  ) {
    if (anchor.x < 0 || anchor.x > size.x || anchor.y < 0 || anchor.y > size.y)
      return;
    for (const distance of [28, 48, 76, 108, 148, 195, 250]) {
      for (const angle of [
        0,
        -Math.PI / 4,
        Math.PI / 4,
        Math.PI,
        (-3 * Math.PI) / 4,
        (3 * Math.PI) / 4,
        -Math.PI / 2,
        Math.PI / 2,
      ]) {
        const cosine = Math.cos(angle);
        const left =
          anchor.x +
          cosine * distance -
          (cosine < -0.1 ? width : cosine > 0.1 ? 0 : width / 2);
        const top = anchor.y + Math.sin(angle) * distance - height / 2;
        const box = { left, top, right: left + width, bottom: top + height };
        if (
          box.left < 10 ||
          box.right > size.x - 10 ||
          box.top < 10 ||
          box.bottom > size.y - 20
        )
          continue;
        if (
          occupied.some(
            (other) =>
              box.left - 6 < other.right &&
              box.right + 6 > other.left &&
              box.top - 5 < other.bottom &&
              box.bottom + 5 > other.top,
          )
        )
          continue;
        occupied.push(box);
        return box;
      }
    }
  }
  const measurements = new Map(
    entries.map(([key, entry]) => {
      const tooltip = entry.destination.getTooltip();
      const element = tooltip?.getElement();
      return [
        key,
        {
          labelWidth: entry.label.offsetWidth,
          labelHeight: entry.label.offsetHeight,
          tooltip,
          element,
          width: element?.offsetWidth ?? 0,
          height: element?.offsetHeight ?? 0,
        },
      ] as const;
    }),
  );
  for (const [key, entry] of entries) {
    const visible = map.hasLayer(entry.marker);
    const anchor = map.latLngToContainerPoint(entry.marker.getLatLng());
    const box = visible
      ? place(
          anchor,
          measurements.get(key)!.labelWidth,
          measurements.get(key)!.labelHeight,
        )
      : undefined;
    entry.label.style.visibility = box ? "visible" : "hidden";
    if (box) {
      entry.label.style.left = `${box.left - anchor.x + 15}px`;
      entry.label.style.top = `${box.top - anchor.y + 20}px`;
      const end = map.containerPointToLatLng([
        Math.max(box.left, Math.min(anchor.x, box.right)),
        Math.max(box.top, Math.min(anchor.y, box.bottom)),
      ]);
      entry.labelLeader.setLatLngs([entry.marker.getLatLng(), end]);
      if (!map.hasLayer(entry.labelLeader)) entry.labelLeader.addTo(map);
    } else entry.labelLeader.remove();
  }
  for (const [key, entry] of entries) {
    const { tooltip, element, width, height } = measurements.get(key)!;
    if (!tooltip || !element || !map.hasLayer(entry.destination)) {
      entry.arrivalLeader.remove();
      continue;
    }
    const anchor = map.latLngToContainerPoint(entry.destination.getLatLng());
    const box = place(anchor, width, height);
    element.style.visibility = box ? "visible" : "hidden";
    if (box) {
      const nextOffset = point(
        box.left - anchor.x - 6,
        box.top - anchor.y + height / 2,
      );
      if (!nextOffset.equals(tooltip.options.offset ?? point(0, 0))) {
        tooltip.options.offset = nextOffset;
        tooltip.update();
      }
      const end = map.containerPointToLatLng([
        Math.max(box.left, Math.min(anchor.x, box.right)),
        Math.max(box.top, Math.min(anchor.y, box.bottom)),
      ]);
      entry.arrivalLeader.setLatLngs([entry.destination.getLatLng(), end]);
      if (!map.hasLayer(entry.arrivalLeader)) entry.arrivalLeader.addTo(map);
    } else entry.arrivalLeader.remove();
  }
}

export default function FleetChart({
  vessels,
  catalog,
  snapshotUTC,
  snapshotReceivedAt,
  inspectorOpen,
  selectedPortID,
  onSelectPort,
  selectedKey,
  layers,
  rosterOpen,
  onSelect,
  onReady,
  onPositionNote,
}: {
  vessels: ChartVessel[];
  catalog?: Catalog;
  snapshotUTC?: string;
  snapshotReceivedAt: number;
  inspectorOpen: boolean;
  selectedPortID: string;
  onSelectPort: (id: string) => void;
  selectedKey: string;
  layers: ChartLayers;
  rosterOpen: boolean;
  onSelect: (key: string) => void;
  onReady: (controls: ChartControls | null) => void;
  onPositionNote: (key: string, text: string) => void;
}) {
  const activity = useMemo(() => chartPortActivity(vessels), [vessels]);
  return (
    <div className={styles.mapFrame}>
      <MapContainer
        className={styles.map}
        bounds={EUROPE}
        boundsOptions={{ padding: [40, 40] }}
        minZoom={2}
        maxZoom={10}
        maxBounds={[
          [-80, -180],
          [85, 180],
        ]}
        maxBoundsViscosity={0.8}
        zoomControl={false}
        attributionControl={false}
        zoomAnimation={false}
        fadeAnimation={false}
        markerZoomAnimation={false}
      >
        <BaseChart />
        <Ports
          ports={catalog?.ports ?? EMPTY_PORTS}
          activity={activity}
          labels={layers.labels}
          selectedPortID={selectedPortID}
          onSelectPort={onSelectPort}
        />
        <VesselLayers
          vessels={vessels}
          snapshotUTC={snapshotUTC}
          snapshotReceivedAt={snapshotReceivedAt}
          inspectorOpen={inspectorOpen}
          layers={layers}
          selectedKey={selectedKey}
          rosterOpen={rosterOpen}
          onSelect={onSelect}
          onReady={onReady}
          onPositionNote={onPositionNote}
        />
        <ChartCoordinates />
        <ScaleControl position="bottomright" imperial={false} />
        <AttributionControl position="bottomright" prefix={false} />
      </MapContainer>
      <div className={styles.north} aria-hidden="true">
        <svg width="18" height="32" viewBox="0 0 18 32">
          <path d="M9 1 3 19l6-4 6 4Z" fill="currentColor" />
          <path d="M9 14v17" stroke="currentColor" />
        </svg>
        <span>North</span>
      </div>
    </div>
  );
}

const BaseChart = memo(function BaseChart() {
  return (
    <>
      <GeoJSON
        data={land as unknown as GeoJsonObject}
        style={LAND_STYLE}
        interactive={false}
        attribution={
          '<a href="https://www.naturalearthdata.com/" target="_blank" rel="noreferrer">Natural Earth</a>; approximate coastline, not for navigation'
        }
      />
      <Polyline
        positions={GRATICULE}
        pathOptions={GRID_STYLE}
        interactive={false}
      />
      {SEA_NAMES.map((sea, index) => (
        <Marker
          key={sea.name}
          position={sea.coordinate}
          icon={SEA_ICONS[index]}
          interactive={false}
          keyboard={false}
        />
      ))}
    </>
  );
});

const Ports = memo(
  function Ports({
    ports,
    activity,
    labels,
    selectedPortID,
    onSelectPort,
  }: {
    ports: Port[];
    activity: Map<string, number>;
    labels: boolean;
    selectedPortID: string;
    onSelectPort: (id: string) => void;
  }) {
    const map = useMap();
    const [zoom, setZoom] = useState(() => map.getZoom());
    useMapEvents({ zoomend: () => setZoom(map.getZoom()) });
    const names = useMemo(() => {
      if (!labels) return new Set<string>();
      const candidates = ports
        .filter(
          (port) =>
            PORT_COORDINATES[port.id] &&
            (port.id === selectedPortID ||
              activity.has(port.id) ||
              zoom >= 6 ||
              (zoom >= 4 && port.is_hub)),
        )
        .sort(
          (left, right) =>
            Number(right.id === selectedPortID) -
              Number(left.id === selectedPortID) ||
            (activity.get(right.id) ?? 0) - (activity.get(left.id) ?? 0) ||
            Number(right.is_hub) - Number(left.is_hub) ||
            left.id.localeCompare(right.id),
        );
      const placed: { x: number; y: number; width: number }[] = [];
      const result = new Set<string>();
      for (const port of candidates) {
        const point = map.project(
          coordinateTuple(PORT_COORDINATES[port.id]),
          zoom,
        );
        const width = port.name.length * 7.3 + 14;
        if (
          placed.some(
            (other) =>
              Math.abs(point.y - other.y) < 24 &&
              Math.abs(point.x - other.x) < (width + other.width) / 2,
          )
        )
          continue;
        placed.push({ x: point.x, y: point.y, width });
        result.add(port.id);
      }
      return result;
    }, [ports, activity, labels, selectedPortID, zoom, map]);
    return ports.map((port) => {
      const coordinate = PORT_COORDINATES[port.id];
      if (!coordinate) return null;
      const count = activity.get(port.id) ?? 0;
      return (
        <CircleMarker
          key={port.id}
          center={coordinateTuple(coordinate)}
          radius={count ? Math.min(8, 3 + Math.sqrt(count) * 1.6) : 2.7}
          eventHandlers={{ click: () => onSelectPort(port.id) }}
          pathOptions={{
            color:
              port.id === selectedPortID
                ? "var(--route)"
                : "var(--chart-coast)",
            fillColor: count ? "var(--ink)" : "var(--chart-land)",
            fillOpacity: 1,
            weight: port.is_hub ? 2.5 : count ? 1.5 : 1,
            dashArray: port.has_shipyard ? "2 2" : undefined,
          }}
        >
          <Tooltip
            key={names.has(port.id) ? "label" : "hover"}
            className={names.has(port.id) ? styles.portName : styles.tooltip}
            permanent={names.has(port.id)}
            direction="top"
            offset={[0, -6]}
          >
            {port.name.replaceAll(" / ", " and ")}
          </Tooltip>
          <Popup className={styles.portPopup}>
            <strong>{port.name.replaceAll(" / ", " and ")}</strong>
            <p>
              {humanize(port.banking_region)}, phase {port.phase}
            </p>
            <p>
              {count} fleet vessel{count === 1 ? "" : "s"} at or routing through
              this port.
            </p>
            <p>
              {port.is_hub ? "Hub port. " : ""}
              {port.has_shipyard ? "Shipyard in catalog. " : ""}Customs{" "}
              {percent(port.customs_bps)}.
            </p>
          </Popup>
        </CircleMarker>
      );
    });
  },
  (previous, next) =>
    previous.ports === next.ports &&
    previous.labels === next.labels &&
    previous.selectedPortID === next.selectedPortID &&
    previous.onSelectPort === next.onSelectPort &&
    previous.activity.size === next.activity.size &&
    [...previous.activity].every(
      ([id, count]) => next.activity.get(id) === count,
    ),
);

function VesselLayers({
  vessels,
  snapshotUTC,
  snapshotReceivedAt,
  inspectorOpen,
  layers,
  selectedKey,
  rosterOpen,
  onSelect,
  onReady,
  onPositionNote,
}: {
  vessels: ChartVessel[];
  snapshotUTC?: string;
  snapshotReceivedAt: number;
  inspectorOpen: boolean;
  layers: ChartLayers;
  selectedKey: string;
  rosterOpen: boolean;
  onSelect: (key: string) => void;
  onReady: (controls: ChartControls | null) => void;
  onPositionNote: (key: string, text: string) => void;
}) {
  const map = useMap();
  const registry = useRef(new Map<string, TrackedVessel>());
  const select = useRef(onSelect);
  const positionNotice = useRef(onPositionNote);
  const initialFit = useRef(false);
  const initialFitFrame = useRef<number | undefined>(undefined);
  const frame = useRef<ChartFrame>({
    vessels,
    layers,
    selectedKey,
    rosterOpen,
    snapshotUTC: undefined,
    serverUTC: Number.NaN,
    monotonicAt: 0,
    wallAt: 0,
    inspectorOpen,
  });
  const arrivalHover = useRef("");

  // Exactly one arrival tag is open at a time: the selected vessel's tag by
  // default, swapped for the hovered or focused vessel's tag while that
  // pointer or keyboard focus lasts. Tags themselves are hover tooltips; this
  // only decides which single tooltip is open.
  const applyArrivalTags = useCallback(() => {
    const hoveredKey = registry.current.has(arrivalHover.current)
      ? arrivalHover.current
      : "";
    const selectedKey = frame.current.selectedKey;
    const activeKey =
      hoveredKey || (registry.current.has(selectedKey) ? selectedKey : "");
    for (const [key, entry] of registry.current) {
      if (!map.hasLayer(entry.destination)) continue;
      if (key === activeKey) {
        entry.arrival.dataset.dimmed = String(key !== selectedKey);
        if (!entry.destination.isTooltipOpen()) entry.destination.openTooltip();
      } else if (entry.destination.isTooltipOpen())
        entry.destination.closeTooltip();
    }
    layoutVesselLabels(map, registry.current, frame.current.selectedKey);
  }, [map]);

  const padding = useCallback((): {
    paddingTopLeft: [number, number];
    paddingBottomRight: [number, number];
  } => {
    const size = map.getSize();
    const root = map.getContainer().closest("main");
    const status = root
      ?.querySelector("[role='status']")
      ?.getBoundingClientRect();
    const roster = root
      ?.querySelector("aside[aria-label='Fleet roster']")
      ?.getBoundingClientRect();
    const inspector = root
      ?.querySelector("aside[aria-label='Ship inspector']")
      ?.getBoundingClientRect();
    const compact = size.x <= 900;
    const top = status?.bottom ? status.bottom + 22 : 120;
    const sheetTop = inspector?.height
      ? inspector.top
      : roster?.height
        ? roster.top
        : size.y;
    return {
      paddingTopLeft: [
        compact ? 30 : roster?.width ? roster.right + 24 : 45,
        top,
      ],
      paddingBottomRight: [
        compact
          ? 30
          : frame.current.inspectorOpen && inspector?.width
            ? size.x - inspector.left + 24
            : 65,
        compact
          ? Math.max(65, Math.min(size.y - sheetTop + 20, size.y - top - 90))
          : 85,
      ],
    };
  }, [map]);

  const fitFleet = useCallback(() => {
    const bounds = latLngBounds([]);
    for (const entry of registry.current.values()) {
      if (!entry.gone && map.hasLayer(entry.marker))
        bounds.extend(entry.marker.getLatLng());
    }
    if (!bounds.isValid()) return false;
    map.fitBounds(bounds, {
      ...padding(),
      maxZoom: 7,
      animate: false,
    });
    return true;
  }, [map, padding]);

  const focusShip = useCallback(
    (key: string) => {
      const entry = registry.current.get(key);
      if (!entry) return;
      // Frame the current position and remaining route only. A completed wake
      // spans the whole voyage and would shrink the fleet to a speck.
      const bounds = latLngBounds([]);
      if (map.hasLayer(entry.marker)) bounds.extend(entry.marker.getLatLng());
      if (map.hasLayer(entry.remaining))
        bounds.extend(entry.remaining.getBounds());
      if (map.hasLayer(entry.destination))
        bounds.extend(entry.destination.getLatLng());
      if (!bounds.isValid()) return;
      const reduced = window.matchMedia(
        "(prefers-reduced-motion: reduce)",
      ).matches;
      const row = frame.current.vessels.find((item) => item.key === key);
      const options = {
        ...padding(),
        maxZoom: row?.detail.ship.status === "sailing" ? 7 : 6,
        animate: !reduced,
        duration: 0.7,
      };
      if (reduced) map.fitBounds(bounds, options);
      else map.flyToBounds(bounds, options);
    },
    [map, padding],
  );

  const update = useCallback(() => {
    const current = frame.current;
    const wallNow = Date.now();
    const serverUTC =
      current.serverUTC +
      Math.max(
        0,
        performance.now() - current.monotonicAt,
        wallNow - current.wallAt,
      );
    const present = new Set<string>();
    const portGroups = new Map<string, string[]>();
    for (const row of current.vessels) {
      const { ship } = row.detail;
      const id =
        ship.status === "sunk"
          ? chartLossPort(row.detail)?.id
          : ship.current_port_id;
      if (ship.status !== "sailing" && id) {
        const group = portGroups.get(id) ?? [];
        group.push(row.key);
        portGroups.set(id, group);
      }
    }
    for (const group of portGroups.values()) group.sort();
    for (const row of current.vessels) {
      present.add(row.key);
      const sailing = row.detail.ship.status === "sailing";
      const gone = row.detail.ship.status === "sunk";
      const wreck = shipIsLoss(row.detail.ship.status, row.detail.ship.fate);
      const selected = row.key === current.selectedKey;
      const old = registry.current.get(row.key);
      const position = chartPosition(
        row.detail,
        row.meta,
        row.clock,
        row.clockMeta,
        serverUTC,
        old?.position,
      );
      if (old) {
        old.position = position;
      }
      if (selected)
        positionNotice.current(
          row.key,
          sailing && !current.layers.estimates
            ? "Estimated positions hidden; the recorded route remains visible."
            : (position?.label ??
                chartUnavailableReason(row.detail, row.clock)),
        );
      // Routes remain visible even when an estimate is unavailable. Their
      // endpoints are recorded route nodes, never a fabricated ship location.
      let entry = old;
      if (!entry && (position || (sailing && row.segments.length))) {
        const root = document.createElement("div");
        root.className = styles.vessel;
        const glyph = document.createElement("span");
        glyph.className = styles.glyph;
        glyph.innerHTML = wreck ? WRECK_GLYPH : SHIP_GLYPH;
        const label = document.createElement("span");
        label.className = styles.shipName;
        root.append(glyph, label);
        const point = position?.coordinate ?? row.segments[0].from;
        const shipMarker = marker(coordinateTuple(point), {
          icon: divIcon({
            className: styles.shipMarker,
            html: root,
            iconSize: [30, 40],
            iconAnchor: [15, 20],
          }),
          keyboard: true,
          riseOnHover: true,
        });
        const tooltip = document.createElement("div");
        shipMarker.bindTooltip(tooltip, {
          className: styles.tooltip,
          direction: "top",
          offset: [0, -22],
        });
        shipMarker.on("click", () => select.current(row.key));
        const revealArrival = () => {
          arrivalHover.current = row.key;
          applyArrivalTags();
        };
        const concealArrival = () => {
          if (arrivalHover.current === row.key) arrivalHover.current = "";
          applyArrivalTags();
        };
        shipMarker.on("mouseover", revealArrival);
        shipMarker.on("mouseout", concealArrival);
        const activate = (event: KeyboardEvent) => {
          if (event.key !== "Enter" && event.key !== " ") return;
          event.preventDefault();
          event.stopPropagation();
          select.current(row.key);
        };
        shipMarker.on("add", () => {
          const element = shipMarker.getElement();
          element?.addEventListener("keydown", activate);
          // Marker focus does not surface as a Leaflet event; keyboard users
          // reveal the arrival tag through the focusable hull element.
          element?.addEventListener("focus", revealArrival);
          element?.addEventListener("blur", concealArrival);
        });
        const arrival = document.createElement("div");
        arrival.className = styles.arrivalTag;
        // Interactive so hovering the destination ring reveals its tag.
        const destination = circleMarker(coordinateTuple(point), {
          radius: 7,
          color: "var(--route)",
          fill: false,
          weight: 1.5,
        });
        destination.on("mouseover", revealArrival);
        destination.on("mouseout", concealArrival);
        entry = {
          marker: shipMarker,
          label,
          tooltip,
          position,
          glyph,
          wreck,
          gone,
          leader: polyline([], {
            color: "var(--ink)",
            weight: 0.7,
            opacity: 0.35,
            interactive: false,
          }),
          labelLeader: polyline([], {
            color: "var(--ink)",
            weight: 0.7,
            opacity: 0.28,
            interactive: false,
          }),
          arrivalLeader: polyline([], {
            color: "var(--route)",
            weight: 0.8,
            opacity: 0.4,
            interactive: false,
          }),
          wake: polyline([], {
            color: "var(--route)",
            weight: 2,
            opacity: 0.2,
            interactive: false,
          }),
          remaining: polyline([], {
            color: "var(--route)",
            weight: 1.5,
            dashArray: "5 8",
            lineCap: "round",
            interactive: false,
          }),
          destination,
          arrival,
        };
        registry.current.set(row.key, entry);
      }
      if (!entry) continue;
      entry.gone = gone;
      if (entry.wreck !== wreck) {
        entry.glyph.innerHTML = wreck ? WRECK_GLYPH : SHIP_GLYPH;
        entry.wreck = wreck;
      }
      const title = `${row.detail.ship.name}. ${row.account.label}, ${row.company}. ${shipStatusLabel(row.detail.ship.status, row.detail.ship.fate)}. ${position?.label ?? chartUnavailableReason(row.detail, row.clock)}. Select to inspect.`;
      if (entry.label.textContent !== row.detail.ship.name)
        entry.label.textContent = row.detail.ship.name;
      if (entry.tooltip.textContent !== title)
        entry.tooltip.textContent = title;
      if (position && (!sailing || current.layers.estimates)) {
        entry.position = position;
        let coordinate = coordinateTuple(position.coordinate);
        const port = gone
          ? chartLossPort(row.detail)?.id
          : row.detail.ship.current_port_id;
        const peers = portGroups.get(port ?? "");
        if (!sailing && peers && peers.length > 1) {
          const point = map.latLngToLayerPoint(coordinate);
          const offset = (peers.indexOf(row.key) - (peers.length - 1) / 2) * 34;
          const displaced = map.layerPointToLatLng(point.add([22, offset]));
          coordinate = [displaced.lat, displaced.lng];
          entry.leader.setLatLngs([
            coordinateTuple(position.coordinate),
            coordinate,
          ]);
          if (!map.hasLayer(entry.leader)) entry.leader.addTo(map);
        } else entry.leader.remove();
        entry.marker.setLatLng(coordinate);
        if (!map.hasLayer(entry.marker)) entry.marker.addTo(map);
        const element = entry.marker.getElement();
        const leg =
          row.detail.ship.current_leg_idx === null
            ? undefined
            : row.detail.route_legs?.[row.detail.ship.current_leg_idx];
        const from = leg ? nodeCoordinate(leg.from_node) : undefined;
        const to = leg ? nodeCoordinate(leg.to_node) : undefined;
        const bearing =
          row.detail.ship.status === "sailing" && from && to
            ? travelBearing(from, to)
            : 0;
        element?.style.setProperty("--bearing", `${bearing}deg`);
        element?.setAttribute(
          "data-state",
          !gone &&
            (row.operation.starving ||
              (row.operation.holdsCurrent &&
                chartBlockingHolds(row.operation.holds, row.detail.ship.status)
                  .length > 0))
            ? "attention"
            : position.kind === "frozen"
              ? "frozen"
              : gone && !wreck
                ? "exit"
                : row.detail.ship.status,
        );
        element?.setAttribute(
          "data-estimated",
          String(position.kind !== "recorded"),
        );
        element?.setAttribute("data-selected", String(selected));
        element?.setAttribute(
          "data-dimmed",
          String(Boolean(current.selectedKey) && !selected),
        );
        element?.setAttribute("aria-label", title);
        element?.setAttribute("aria-pressed", String(selected));
        entry.marker.setZIndexOffset(selected ? 1200 : 500);
      } else {
        entry.marker.remove();
        entry.leader.remove();
      }
      if (sailing && current.layers.routes && row.segments.length) {
        // Hidden estimates must not split or animate the route: pass no split
        // point while Estimated positions is off. Completed legs still fill
        // the wake; they are recorded route, not an estimate.
        const splitAt = current.layers.estimates ? position : undefined;
        if (
          entry.segments !== row.segments ||
          entry.splitLeg !== row.detail.ship.current_leg_idx ||
          entry.splitLatitude !== splitAt?.coordinate.latitude ||
          entry.splitLongitude !== splitAt?.coordinate.longitude
        ) {
          const split = splitChartRoute(
            row.segments,
            row.detail.ship.current_leg_idx,
            splitAt,
          );
          entry.wake.setLatLngs(routePoints(split.wake));
          entry.remaining.setLatLngs(routePoints(split.remaining));
          entry.segments = row.segments;
          entry.splitLeg = row.detail.ship.current_leg_idx;
          entry.splitLatitude = splitAt?.coordinate.latitude;
          entry.splitLongitude = splitAt?.coordinate.longitude;
        }
        const emphasized = !current.selectedKey || selected;
        entry.wake.setStyle({
          opacity: emphasized ? 0.22 : 0.08,
          weight: selected ? 3 : 1.7,
        });
        entry.remaining.setStyle({
          opacity: emphasized ? 0.8 : 0.25,
          weight: selected ? 2.8 : 1.5,
        });
        if (!map.hasLayer(entry.wake)) entry.wake.addTo(map);
        if (!map.hasLayer(entry.remaining)) entry.remaining.addTo(map);
        const final = nodeCoordinate(
          row.detail.route_legs?.at(-1)?.to_node ?? "",
        );
        if (final) {
          entry.destination.setLatLng(coordinateTuple(final));
          entry.destination.setStyle({ opacity: emphasized ? 0.9 : 0.3 });
          if (!map.hasLayer(entry.destination)) entry.destination.addTo(map);
          const eta = voyageETA(row.detail);
          // Compact relative countdown for the destination ring tag; the
          // helper's "in " prefix is display-only. The scheduled UTC detail
          // stays in the tag title for hover and accessibility.
          const tag = chartETA(
            row.detail,
            row.clock,
            row.clockMeta,
            row.meta,
            serverUTC,
          ).replace(/^in /, "");
          if (entry.arrival.textContent !== tag)
            entry.arrival.textContent = tag;
          entry.arrival.title =
            eta === undefined
              ? "Full voyage arrival not recorded"
              : `Scheduled ${dateTime(new Date(eta).toISOString())}. Arrival is not confirmed.`;
          if (!entry.destination.getTooltip()) {
            // Hover tooltip: applyArrivalTags holds the selected vessel's tag
            // open and keeps every other one hidden until hovered or focused.
            entry.destination.bindTooltip(entry.arrival, {
              direction: "right",
              offset: [12, 0],
              className: styles.destinationLabel,
            });
          }
        } else entry.destination.remove();
      } else {
        entry.wake.remove();
        entry.remaining.remove();
        entry.destination.remove();
      }
    }
    for (const [key, entry] of registry.current)
      if (!present.has(key)) {
        entry.marker.remove();
        entry.leader.remove();
        entry.labelLeader.remove();
        entry.arrivalLeader.remove();
        entry.wake.remove();
        entry.remaining.remove();
        entry.destination.remove();
        registry.current.delete(key);
      }
    // Sync which arrival tag is open, then place labels and leaders.
    applyArrivalTags();
    if (
      !initialFit.current &&
      initialFitFrame.current === undefined &&
      registry.current.size > 0
    ) {
      initialFitFrame.current = requestAnimationFrame(() => {
        initialFitFrame.current = undefined;
        initialFit.current = fitFleet();
      });
    }
  }, [map, fitFleet, applyArrivalTags]);

  useEffect(() => {
    select.current = onSelect;
    positionNotice.current = onPositionNote;
    const current = frame.current;
    if (current.snapshotUTC !== snapshotUTC) {
      const wall = Date.now();
      const monotonic = performance.now();
      const previous =
        current.serverUTC +
        Math.max(0, wall - current.wallAt, monotonic - current.monotonicAt);
      const received =
        Date.parse(snapshotUTC ?? "") + Math.max(0, wall - snapshotReceivedAt);
      current.snapshotUTC = snapshotUTC;
      current.serverUTC = Number.isFinite(previous)
        ? Math.max(previous, received)
        : received;
      current.monotonicAt = monotonic;
      current.wallAt = wall;
    }
    current.vessels = vessels;
    current.layers = layers;
    current.selectedKey = selectedKey;
    current.rosterOpen = rosterOpen;
    current.inspectorOpen = inspectorOpen;
    if (!document.hidden) update();
  }, [
    vessels,
    layers,
    selectedKey,
    rosterOpen,
    snapshotUTC,
    inspectorOpen,
    snapshotReceivedAt,
    onSelect,
    onPositionNote,
    update,
  ]);

  useEffect(() => {
    let timer: number | undefined;
    const tracked = registry.current;
    const container = map.getContainer();
    container.setAttribute("role", "region");
    container.setAttribute(
      "aria-label",
      "Fleet chart. Arrow keys pan, plus and minus zoom. Hollow vessels are estimated sea positions. Select a vessel or use the fleet roster for recorded details.",
    );
    const observer = new ResizeObserver(() => {
      map.invalidateSize({ pan: false, animate: false });
    });
    observer.observe(container);
    const visibilityChanged = () => {
      window.clearInterval(timer);
      if (!document.hidden) {
        update();
        timer = window.setInterval(update, 1000);
      }
    };
    document.addEventListener("visibilitychange", visibilityChanged);
    map.on("zoomend moveend", update);
    visibilityChanged();
    return () => {
      window.clearInterval(timer);
      if (initialFitFrame.current !== undefined)
        cancelAnimationFrame(initialFitFrame.current);
      initialFitFrame.current = undefined;
      initialFit.current = false;
      observer.disconnect();
      document.removeEventListener("visibilitychange", visibilityChanged);
      map.off("zoomend moveend", update);
      for (const entry of tracked.values()) {
        entry.marker.remove();
        entry.leader.remove();
        entry.labelLeader.remove();
        entry.arrivalLeader.remove();
        entry.wake.remove();
        entry.remaining.remove();
        entry.destination.remove();
      }
      tracked.clear();
    };
  }, [map, update]);

  useEffect(() => {
    onReady({
      fitFleet,
      focusShip,
      zoom: (change) => map.setZoom(map.getZoom() + change, { animate: false }),
      focusPort: (id) => {
        const coordinate = PORT_COORDINATES[id];
        if (coordinate)
          map.setView(coordinateTuple(coordinate), Math.max(map.getZoom(), 6), {
            animate: false,
          });
      },
    });
    return () => onReady(null);
  }, [map, onReady, fitFleet, focusShip]);

  return null;
}

function ChartCoordinates() {
  const map = useMap();
  const [labels, setLabels] = useState<
    { key: string; text: string; left: number; top: number }[]
  >([]);
  useEffect(() => {
    function updateLabels() {
      const bounds = map.getBounds();
      const size = map.getSize();
      const step = map.getZoom() >= 4 ? 10 : 30;
      const next = [];
      for (let latitude = -60; latitude <= 70; latitude += step) {
        const point = map.latLngToContainerPoint([latitude, bounds.getWest()]);
        if (point.y > 160 && point.y < size.y - 70)
          next.push({
            key: `lat:${latitude}`,
            text: `${Math.abs(latitude)}°${latitude < 0 ? "S" : latitude > 0 ? "N" : ""}`,
            left: 8,
            top: point.y,
          });
      }
      for (let longitude = -180; longitude <= 180; longitude += step) {
        const point = map.latLngToContainerPoint([
          bounds.getNorth(),
          longitude,
        ]);
        if (point.x > 50 && point.x < size.x - 55)
          next.push({
            key: `lng:${longitude}`,
            text: `${Math.abs(longitude)}°${longitude < 0 ? "W" : longitude > 0 ? "E" : ""}`,
            left: point.x,
            top: size.y - 70,
          });
      }
      setLabels(next);
    }
    updateLabels();
    map.on("moveend resize", updateLabels);
    return () => {
      map.off("moveend resize", updateLabels);
    };
  }, [map]);
  return (
    <div className={styles.coordinates} aria-hidden="true">
      {labels.map((label) => (
        <span key={label.key} style={{ left: label.left, top: label.top }}>
          {label.text}
        </span>
      ))}
    </div>
  );
}
