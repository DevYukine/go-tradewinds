"use client";

import type { GeoJsonObject } from "geojson";
import {
  divIcon,
  latLngBounds,
  marker,
  type LatLngTuple,
  type Map as LeafletMap,
  type Marker as LeafletMarker,
} from "leaflet";
import {
  Anchor,
  CircleHelp,
  Crosshair,
  Hammer,
  Minus,
  Plus,
  Navigation2,
  Snowflake,
} from "lucide-react";
import {
  memo,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type RefObject,
} from "react";
import {
  AttributionControl,
  GeoJSON,
  MapContainer,
  Marker,
  Polyline,
  ScaleControl,
  Tooltip,
  useMap,
  useMapEvents,
} from "react-leaflet";
import { dataset, fleet, metadata, shipSource } from "@/lib/data";
import { humanize } from "@/lib/format";
import {
  PORT_COORDINATES,
  nodeCoordinate,
  routeSegments,
  shipPosition,
  type ChartPosition,
  travelBearing,
} from "@/lib/geography";
import type {
  Account,
  Catalog,
  Clock,
  CompanyProfile,
  DatasetMeta,
  Port,
  ShipDetail,
} from "@/lib/types";
import { useUI } from "@/lib/ui-store";
import styles from "../fleet.module.css";
import land from "./ne_50m_land.json";
import "leaflet/dist/leaflet.css";

const EUROPE: [LatLngTuple, LatLngTuple] = [
  [35, -13],
  [63, 31],
];
const GRATICULE: LatLngTuple[][] = [
  ...Array.from({ length: 15 }, (_, index): LatLngTuple[] => {
    const latitude = -70 + index * 10;
    return [
      [latitude, -180],
      [latitude, 180],
    ];
  }),
  ...Array.from({ length: 37 }, (_, index): LatLngTuple[] => {
    const longitude = -180 + index * 10;
    return [
      [-80, longitude],
      [85, longitude],
    ];
  }),
];
const LAND_STYLE = { className: styles.land };
const GRATICULE_STYLE = {
  className: styles.graticule,
  color: "var(--muted)",
  weight: 0.75,
  opacity: 0.22,
};
const PORT_SYMBOLS = {
  hub: '<path d="M12 3v18m-5-9H3a9 9 0 0 0 18 0h-4M8 7h8"/><circle cx="12" cy="4" r="2"/>',
  yard: '<path d="m15 3 6 6-4 4-3-3-9 11-3-3 11-9-3-3z"/>',
  port: '<circle cx="12" cy="12" r="3" fill="currentColor"/>',
};
const PORT_ICONS = Object.fromEntries(
  Object.entries(PORT_SYMBOLS).map(([kind, symbol]) => [
    kind,
    divIcon({
      className: `${styles.portMarker} ${styles[kind]}`,
      html: `<span class="${styles.portDisc}"><svg viewBox="0 0 24 24" aria-hidden="true">${symbol}</svg></span>`,
      iconSize: [44, 44],
      iconAnchor: [22, 22],
    }),
  ]),
);
const SHIP_ICON = divIcon({
  className: styles.shipMarker,
  html: `<span class="${styles.shipPulse}" aria-hidden="true"></span><svg class="${styles.vessel}" viewBox="0 0 32 40" aria-hidden="true"><path class="${styles.vesselHull}" d="M16 2C11 7 8 15 8 24l3 11h10l3-11c0-9-3-17-8-22Z"/><path class="${styles.vesselDeck}" d="M16 9v23M10 18h12M10 25h12M13 31h6"/><path class="${styles.vesselSail}" d="M16 11 9 20h7zm2 9 5 8h-5z"/></svg>`,
  iconSize: [44, 44],
  iconAnchor: [22, 22],
});
const EMPTY_PORTS: Port[] = [];

interface ChartShip {
  key: string;
  account: Account;
  detail: ShipDetail;
  meta: DatasetMeta;
  clock?: Clock;
  clockMeta: DatasetMeta;
  identity: string;
}

interface TrackedShip {
  marker: LeafletMarker;
  tooltip: HTMLElement;
  position: ChartPosition;
  label: string;
}

interface LegendCounts {
  frozen: number;
  unavailable: number;
}

const EMPTY_LEGEND: LegendCounts = {
  frozen: 0,
  unavailable: 0,
};

const INITIAL_STATUS = "Waiting for usable ship and clock recordings.";

export default function FleetChart({
  accounts,
  catalog,
  snapshotUTC,
}: {
  accounts: Account[];
  catalog?: Catalog;
  snapshotUTC?: string;
}) {
  const map = useRef<LeafletMap | null>(null);
  const positions = useRef(new Map<string, ChartPosition>());
  const [positionSummary, setPositionSummary] = useState(INITIAL_STATUS);
  const [legendCounts, setLegendCounts] = useState(EMPTY_LEGEND);
  const [notice, setNotice] = useState("");
  // Stable identity: ShipMarkers reports ticker-derived summaries through it,
  // and React bails out when the string is unchanged, so the one-second
  // refresh still touches only Leaflet markers.
  const handlePositionStatus = useCallback(
    (status: string, counts: LegendCounts) => {
      setPositionSummary(status);
      setLegendCounts((previous) =>
        previous.frozen === counts.frozen &&
        previous.unavailable === counts.unavailable
          ? previous
          : counts,
      );
    },
    [],
  );
  const selectedPortID = useUI((state) => state.selectedPortID);
  const selectedShipID = useUI((state) => state.selectedShipID);
  const selectedShipAccountID = useUI((state) => state.selectedShipAccountID);
  const selectPort = useUI((state) => state.selectPort);
  const ports = catalog?.ports ?? EMPTY_PORTS;
  const selectedDetail = useMemo(
    () =>
      fleet(
        accounts.find((account) => account.id === selectedShipAccountID),
      ).find((detail) => detail.ship.id === selectedShipID),
    [accounts, selectedShipAccountID, selectedShipID],
  );
  const segments = useMemo(
    () => (selectedDetail ? routeSegments(selectedDetail) : []),
    [selectedDetail],
  );
  const relevantPortIDs = useMemo(() => {
    const ids = new Set<string>();
    for (const account of accounts) {
      for (const { ship } of fleet(account)) {
        if (ship.current_port_id) {
          ids.add(ship.current_port_id);
        }
      }
    }
    for (const leg of selectedDetail?.route_legs ?? []) {
      ids.add(leg.from_node);
      ids.add(leg.to_node);
    }
    return ids;
  }, [accounts, selectedDetail]);
  const [initialBounds] = useState(() => {
    const points: LatLngTuple[] = [];
    for (const account of accounts) {
      for (const detail of fleet(account)) {
        if (detail.ship.status === "sunk") {
          continue;
        }
        const leg = detail.route_legs?.[detail.ship.current_leg_idx];
        const nodes =
          detail.ship.status === "sailing"
            ? [leg?.from_node, leg?.to_node]
            : [detail.ship.current_port_id];
        for (const node of nodes) {
          const coordinate = node ? nodeCoordinate(node) : undefined;
          if (coordinate) {
            points.push([coordinate.latitude, coordinate.longitude]);
          }
        }
      }
    }
    return points.length ? latLngBounds(points) : latLngBounds(EUROPE);
  });

  function fitFleet() {
    const points = [...positions.current.values()].map(
      ({ coordinate }): LatLngTuple => [
        coordinate.latitude,
        coordinate.longitude,
      ],
    );
    if (points.length === 0) {
      setNotice(
        "No fleet position is available. Open the vessel ledger to inspect recorded facts.",
      );
      return;
    }
    map.current?.fitBounds(latLngBounds(points), {
      padding: [40, 40],
      maxZoom: 7,
      animate: false,
    });
    setNotice("");
  }

  function focusPort(id: string) {
    selectPort(id);
    const coordinate = PORT_COORDINATES[id];
    const instance = map.current;
    if (coordinate && instance) {
      instance.setView(
        [coordinate.latitude, coordinate.longitude],
        Math.max(6, instance.getZoom()),
        { animate: false },
      );
    }
  }

  return (
    <>
      <div className={styles.chartToolbar}>
        <div
          className={styles.chartActions}
          role="group"
          aria-label="Chart view"
        >
          <button className={styles.button} onClick={fitFleet}>
            <Crosshair size={15} strokeWidth={1.7} aria-hidden="true" /> Fit
            fleet
          </button>
          <div
            className={styles.zoomActions}
            role="group"
            aria-label="Chart zoom"
          >
            <button
              className={styles.iconButton}
              onClick={() => map.current?.zoomOut(1, { animate: false })}
              aria-label="Zoom chart out"
            >
              <Minus size={15} strokeWidth={1.7} aria-hidden="true" />
            </button>
            <button
              className={styles.iconButton}
              onClick={() => map.current?.zoomIn(1, { animate: false })}
              aria-label="Zoom chart in"
            >
              <Plus size={15} strokeWidth={1.7} aria-hidden="true" />
            </button>
          </div>
        </div>
        <label className={styles.portSelect}>
          Port
          <select
            value={selectedPortID}
            onChange={(event) => focusPort(event.target.value)}
          >
            <option value="">Choose a catalog port</option>
            {selectedPortID &&
              !ports.some((port) => port.id === selectedPortID) && (
                <option value={selectedPortID}>
                  {humanize(selectedPortID)} (not in this catalog)
                </option>
              )}
            {[...ports]
              .sort((left, right) => left.name.localeCompare(right.name))
              .map((port) => (
                <option key={port.id} value={port.id}>
                  {port.name}
                  {!PORT_COORDINATES[port.id] ? " (position unavailable)" : ""}
                </option>
              ))}
          </select>
        </label>
      </div>
      {notice && (
        <p className={styles.note} role="status">
          {notice}
        </p>
      )}
      <div className={styles.mapFrame}>
        <MapContainer
          ref={map}
          className={styles.map}
          bounds={initialBounds}
          boundsOptions={{ padding: [35, 35], maxZoom: 6 }}
          minZoom={1}
          maxZoom={10}
          maxBounds={[
            [-85, -220],
            [85, 220],
          ]}
          maxBoundsViscosity={0.8}
          zoomControl={false}
          attributionControl={false}
          zoomAnimation={false}
          fadeAnimation={false}
          markerZoomAnimation={false}
        >
          <GeoJSON
            data={land as unknown as GeoJsonObject}
            style={LAND_STYLE}
            interactive={false}
            attribution={
              'Made with <a href="https://www.naturalearthdata.com/">Natural Earth</a>'
            }
          />
          <Polyline
            positions={GRATICULE}
            pathOptions={GRATICULE_STYLE}
            interactive={false}
          />
          {segments.map(({ from, to, index }) => {
            const current =
              selectedDetail?.ship.status === "sailing" &&
              index === selectedDetail.ship.current_leg_idx;
            return (
              <Polyline
                key={`${selectedShipAccountID}:${selectedShipID}:${index}:${current}`}
                positions={[
                  [from.latitude, from.longitude],
                  [to.latitude, to.longitude],
                ]}
                pathOptions={{
                  className: `${styles.route} ${current ? styles.currentRoute : ""}`,
                  color: "var(--route)",
                  weight: current ? 3 : 2,
                  dashArray: "6 8",
                  lineCap: "round",
                }}
              >
                <Tooltip className={styles.markerTooltip} sticky>
                  Recorded route leg {index + 1}. Connecting geometry is
                  schematic, not a recorded track.
                </Tooltip>
              </Polyline>
            );
          })}
          {segments.map(({ from, to, index }) => (
            <Marker
              key={`direction:${index}`}
              position={[
                (from.latitude + to.latitude) / 2,
                (from.longitude + to.longitude) / 2,
              ]}
              icon={divIcon({
                className: styles.routeDirection,
                html: `<svg viewBox="0 0 20 20" style="transform:rotate(${travelBearing(from, to)}deg)" aria-hidden="true"><path d="m5 13 5-6 5 6"/></svg>`,
                iconSize: [20, 20],
                iconAnchor: [10, 10],
              })}
              interactive={false}
              keyboard={false}
            />
          ))}
          <Ports
            ports={ports}
            selectedPortID={selectedPortID}
            relevantPortIDs={relevantPortIDs}
          />
          <ShipMarkers
            accounts={accounts}
            snapshotUTC={snapshotUTC}
            positions={positions}
            onStatusChange={handlePositionStatus}
          />
          <ViewportSize />
          <ChartCoordinates />
          <ScaleControl position="bottomleft" imperial={false} />
          <AttributionControl position="bottomright" prefix={false} />
        </MapContainer>
      </div>
      <div className={styles.chartLegend} aria-label="Chart legend">
        <span>
          <Navigation2 size={15} strokeWidth={1.7} aria-hidden="true" />
          Estimated at sea
        </span>
        <span className={styles.legendBerthed}>
          <Anchor size={15} strokeWidth={1.7} aria-hidden="true" /> Berthed
        </span>
        <span className={styles.legendAnchored}>
          <Anchor size={15} strokeWidth={1.7} aria-hidden="true" /> Anchored
        </span>
        {legendCounts.frozen > 0 && (
          <span className={styles.legendFrozen}>
            <Snowflake size={15} strokeWidth={1.7} aria-hidden="true" />
            Frozen estimate
          </span>
        )}
        {legendCounts.unavailable > 0 && (
          <span className={styles.legendFrozen}>
            <CircleHelp size={15} strokeWidth={1.7} aria-hidden="true" />
            Position unavailable
          </span>
        )}
        <span>
          <Anchor size={15} strokeWidth={1.7} aria-hidden="true" /> Hub
        </span>
        <span className={styles.legendAnchored}>
          <Hammer size={15} strokeWidth={1.7} aria-hidden="true" /> Shipyard
        </span>
        <span>
          <span className={styles.legendPort} /> Port (rings show phase)
        </span>
      </div>
      <p className={styles.note} role="status">
        {positionSummary}
      </p>
    </>
  );
}

const Ports = memo(function Ports({
  ports,
  selectedPortID,
  relevantPortIDs,
}: {
  ports: Port[];
  selectedPortID: string;
  relevantPortIDs: Set<string>;
}) {
  const map = useMap();
  const [zoom, setZoom] = useState(() => map.getZoom());
  useMapEvents({ zoomend: () => setZoom(map.getZoom()) });
  const labeledPorts = useMemo(() => {
    const candidates = ports
      .filter(
        (port) =>
          PORT_COORDINATES[port.id] &&
          (port.id === selectedPortID ||
            relevantPortIDs.has(port.id) ||
            zoom >= 9 ||
            (zoom >= 6 && port.is_hub)),
      )
      .map((port) => ({
        port,
        priority:
          port.id === selectedPortID
            ? 0
            : relevantPortIDs.has(port.id)
              ? 1
              : port.is_hub
                ? 2
                : 3,
      }))
      .sort(
        (left, right) =>
          left.priority - right.priority ||
          left.port.id.localeCompare(right.port.id),
      );
    const labeled = new Set<string>();
    const placed: {
      left: number;
      right: number;
      top: number;
      bottom: number;
    }[] = [];
    for (const { port } of candidates) {
      const coordinate = PORT_COORDINATES[port.id];
      const point = map.project(
        [coordinate.latitude, coordinate.longitude],
        zoom,
      );
      // Conservative bounds for the 11px port label include breathing room.
      // Projected collisions are pan-invariant, so only zoom needs tracking.
      const halfWidth = Math.max(48, port.name.length * 8 + 16) / 2;
      const box = {
        left: point.x - halfWidth,
        right: point.x + halfWidth,
        top: point.y - 40,
        bottom: point.y - 8,
      };
      if (
        placed.some(
          (other) =>
            box.left < other.right &&
            box.right > other.left &&
            box.top < other.bottom &&
            box.bottom > other.top,
        )
      ) {
        continue;
      }
      labeled.add(port.id);
      placed.push(box);
    }
    return labeled;
  }, [map, ports, relevantPortIDs, selectedPortID, zoom]);
  return ports.map((port) =>
    PORT_COORDINATES[port.id] ? (
      <PortMarker
        key={port.id}
        port={port}
        selected={port.id === selectedPortID}
        showLabel={labeledPorts.has(port.id)}
        zoom={zoom}
      />
    ) : null,
  );
});

const PortMarker = memo(function PortMarker({
  port,
  selected,
  showLabel,
  zoom,
}: {
  port: Port;
  selected: boolean;
  showLabel: boolean;
  zoom: number;
}) {
  const reference = useRef<LeafletMarker | null>(null);
  const kind = port.is_hub ? "hub" : port.has_shipyard ? "yard" : "port";
  const selectPort = useUI((state) => state.selectPort);
  const coordinate = PORT_COORDINATES[port.id];
  const handlers = useMemo(
    () => ({ click: () => selectPort(port.id) }),
    [port.id, selectPort],
  );
  const label = `${port.name}. ${kind === "hub" ? "Hub port" : kind === "yard" ? "Shipyard port" : "Port"}. Phase ${port.phase}. ${humanize(port.banking_region)}. Select port details.`;

  useEffect(() => {
    const element = reference.current?.getElement();
    element?.classList.toggle(styles.portSelected, selected);
    element?.setAttribute("aria-label", label);
    element?.classList.toggle(styles.portDetailed, zoom >= 5 || selected);
    element?.style.setProperty("--port-phase", String(Math.max(1, port.phase)));
    element?.setAttribute("title", label);
  }, [selected, label, zoom, port.phase]);

  return (
    <Marker
      ref={reference}
      position={[coordinate.latitude, coordinate.longitude]}
      icon={PORT_ICONS[kind]}
      title={label}
      alt={label}
      eventHandlers={handlers}
      zIndexOffset={selected ? 100 : 0}
    >
      <Tooltip
        key={showLabel ? "label" : "tooltip"}
        className={`${styles.markerTooltip} ${showLabel ? styles.portLabel : ""}`}
        direction="top"
        offset={[0, -8]}
        permanent={showLabel}
      >
        <strong>{port.name}</strong>
        {!showLabel && (
          <span className={styles.cellSubtext}>
            {kind === "hub" ? "Hub" : kind === "yard" ? "Shipyard" : "Port"},
            phase {port.phase}
          </span>
        )}
      </Tooltip>
    </Marker>
  );
});

function ShipMarkers({
  accounts,
  snapshotUTC,
  positions,
  onStatusChange,
}: {
  accounts: Account[];
  snapshotUTC?: string;
  positions: RefObject<Map<string, ChartPosition>>;
  onStatusChange: (status: string, counts: LegendCounts) => void;
}) {
  const map = useMap();
  const selectShip = useUI((state) => state.selectShip);
  const selectedShipID = useUI((state) => state.selectedShipID);
  const selectedShipAccountID = useUI((state) => state.selectedShipAccountID);
  const tracked = useRef(new Map<string, TrackedShip>());
  const source = useRef({
    snapshotUTC: undefined as string | undefined,
    serverUTC: Number.NaN,
    monotonicAt: 0,
    ships: [] as ChartShip[],
    selectedShipID: "",
    selectedShipAccountID: "",
  });

  // Markers and tooltips are imperative Leaflet bridges mutated in place on a
  // timer. The human-readable summary flows through onStatusChange so React
  // owns that text and re-renders only when it actually changes.
  const update = useCallback(
    (positionMap: Map<string, ChartPosition>) => {
      const frame = source.current;
      const serverUTC = frame.serverUTC + performance.now() - frame.monotonicAt;
      const present = new Set<string>();
      const counts = {
        recorded: 0,
        estimated: 0,
        awaiting: 0,
        frozen: 0,
        unavailable: 0,
        sunk: 0,
      };
      const legendCounts = { ...EMPTY_LEGEND };
      for (const row of frame.ships) {
        present.add(row.key);
        const previous = tracked.current.get(row.key);
        const position = shipPosition(
          row.detail,
          row.meta,
          row.clock,
          row.clockMeta,
          serverUTC,
          previous?.position,
        );
        if (!position) {
          legendCounts.unavailable += 1;
          if (row.detail.ship.status === "sunk") {
            counts.sunk += 1;
          } else {
            counts.unavailable += 1;
          }
          previous?.marker.remove();
          tracked.current.delete(row.key);
          positionMap.delete(row.key);
          continue;
        }
        counts[position.kind] += 1;
        if (position.kind === "frozen") {
          legendCounts.frozen += 1;
        }
        const starving =
          row.detail.voyage?.legs?.some((leg) => leg.starving) ?? false;
        const label = `Inspect ${row.detail.ship.name}, ${humanize(row.detail.ship.status)}. ${row.identity}. ${position.label}${starving ? ". Starvation recorded in voyage estimate" : ""}`;
        let entry = previous;
        if (!entry) {
          const tooltip = document.createElement("div");
          tooltip.textContent = label;
          const shipMarker = marker(
            [position.coordinate.latitude, position.coordinate.longitude],
            {
              icon: SHIP_ICON,
              title: label,
              alt: label,
              keyboard: true,
              riseOnHover: true,
            },
          );
          shipMarker.bindTooltip(tooltip, {
            className: styles.markerTooltip,
            direction: "top",
            offset: [0, -15],
          });
          shipMarker.on("click", () =>
            selectShip(row.detail.ship.id, row.account.id),
          );
          shipMarker.addTo(map);
          entry = { marker: shipMarker, tooltip, position, label };
          tracked.current.set(row.key, entry);
        } else {
          if (
            position.coordinate.latitude !==
              entry.position.coordinate.latitude ||
            position.coordinate.longitude !==
              entry.position.coordinate.longitude
          ) {
            entry.marker.setLatLng([
              position.coordinate.latitude,
              position.coordinate.longitude,
            ]);
          }
          if (entry.label !== label) {
            entry.tooltip.textContent = label;
            entry.label = label;
          }
          entry.position = position;
        }
        const selected =
          row.detail.ship.id === frame.selectedShipID &&
          row.account.id === frame.selectedShipAccountID;
        const element = entry.marker.getElement();
        const leg = row.detail.route_legs?.[row.detail.ship.current_leg_idx];
        const from = leg ? nodeCoordinate(leg.from_node) : undefined;
        const to = leg ? nodeCoordinate(leg.to_node) : undefined;
        const bearing =
          row.detail.ship.status === "sailing" && from && to
            ? travelBearing(from, to)
            : 0;
        element?.style.setProperty("--bearing", `${bearing}deg`);
        element?.setAttribute(
          "data-status",
          position.kind === "frozen" ? "frozen" : row.detail.ship.status,
        );
        element?.classList.toggle(
          styles.shipEstimated,
          position.kind === "estimated" || position.kind === "awaiting",
        );
        element?.classList.toggle(styles.shipSelected, selected);
        element?.classList.toggle(styles.shipWarning, starving);
        element?.setAttribute("aria-label", label);
        element?.setAttribute("aria-pressed", String(selected));
        element?.setAttribute("title", label);
        entry.marker.setZIndexOffset(selected ? 1000 : 400);
        positionMap.set(row.key, position);
      }
      for (const [key, entry] of tracked.current) {
        if (!present.has(key)) {
          entry.marker.remove();
          tracked.current.delete(key);
          positionMap.delete(key);
        }
      }
      const summary = [
        counts.recorded ? `${counts.recorded} at recorded ports` : "",
        counts.estimated ? `${counts.estimated} estimated at sea` : "",
        counts.awaiting
          ? `${counts.awaiting} at scheduled leg arrival, awaiting recording`
          : "",
        counts.frozen ? `${counts.frozen} estimates frozen` : "",
      ]
        .filter(Boolean)
        .join("; ");
      const description = `${summary || "No ship position is available"}.${counts.unavailable ? ` Position unavailable for ${counts.unavailable} ${counts.unavailable === 1 ? "ship" : "ships"}: estimates need usable ship and clock recordings, valid times and known endpoints.` : ""}${counts.sunk ? ` ${counts.sunk} sunk ${counts.sunk === 1 ? "ship has" : "ships have"} no recorded sinking position.` : ""}`;
      onStatusChange(description, legendCounts);
    },
    [map, selectShip, onStatusChange],
  );
  useEffect(() => {
    const frame = source.current;
    // A repeated or failed poll must not make an older clock observation new.
    if (frame.snapshotUTC !== snapshotUTC) {
      frame.snapshotUTC = snapshotUTC;
      frame.serverUTC = Date.parse(snapshotUTC ?? "");
      frame.monotonicAt = performance.now();
    }
    frame.ships = accounts.flatMap((account) => {
      const clock = dataset<Clock>(account, "clock");
      const clockMeta = metadata(account, "clock");
      const company =
        dataset<CompanyProfile>(account, "profile")?.company.name ??
        account.company_id;
      return fleet(account).map((detail) => ({
        key: `${account.id}:${detail.ship.id}`,
        account,
        detail,
        meta: metadata(account, shipSource(account, detail.ship.id)),
        clock,
        clockMeta,
        identity: `${account.label}, ${company}`,
      }));
    });
    frame.selectedShipID = selectedShipID;
    frame.selectedShipAccountID = selectedShipAccountID;
    if (!document.hidden) {
      update(positions.current);
    }
  }, [
    accounts,
    snapshotUTC,
    selectedShipID,
    selectedShipAccountID,
    positions,
    update,
  ]);

  useEffect(() => {
    let timer: number | undefined;
    const registry = tracked.current;
    const latestPositions = positions.current;
    function visibilityChanged() {
      clearInterval(timer);
      timer = undefined;
      if (!document.hidden) {
        update(latestPositions);
        timer = window.setInterval(() => update(latestPositions), 1000);
      }
    }
    document.addEventListener("visibilitychange", visibilityChanged);
    visibilityChanged();
    return () => {
      clearInterval(timer);
      document.removeEventListener("visibilitychange", visibilityChanged);
      for (const entry of registry.values()) {
        entry.marker.remove();
      }
      registry.clear();
      latestPositions.clear();
    };
  }, [positions, update]);

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
      for (let latitude = -60; latitude <= 60; latitude += step) {
        const point = map.latLngToContainerPoint([latitude, bounds.getWest()]);
        if (point.y > 24 && point.y < size.y - 45) {
          next.push({
            key: `lat:${latitude}`,
            text: `${Math.abs(latitude)}°${latitude < 0 ? "S" : latitude > 0 ? "N" : ""}`,
            left: 8,
            top: point.y,
          });
        }
      }
      for (let longitude = -180; longitude <= 180; longitude += step) {
        const point = map.latLngToContainerPoint([
          bounds.getNorth(),
          longitude,
        ]);
        if (point.x > 55 && point.x < size.x - 45) {
          next.push({
            key: `lng:${longitude}`,
            text: `${Math.abs(longitude)}°${longitude < 0 ? "W" : longitude > 0 ? "E" : ""}`,
            left: point.x,
            top: 10,
          });
        }
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
    <div className={styles.chartCoordinates} aria-hidden="true">
      {labels.map((label) => (
        <span key={label.key} style={{ left: label.left, top: label.top }}>
          {label.text}
        </span>
      ))}
    </div>
  );
}

function ViewportSize() {
  const map = useMap();
  useEffect(() => {
    const container = map.getContainer();
    container.setAttribute("role", "region");
    container.setAttribute(
      "aria-label",
      "Fleet chart. Arrow keys pan, plus and minus zoom. Ship positions at sea are estimates. Select a ship marker to inspect recorded facts; the Fleet view lists every ship.",
    );
    const observer = new ResizeObserver(() =>
      map.invalidateSize({ pan: false, animate: false }),
    );
    observer.observe(map.getContainer());
    return () => observer.disconnect();
  }, [map]);
  return null;
}
