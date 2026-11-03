"use client";

import {
  ArrowLeft,
  Anchor,
  Check,
  ChevronDown,
  ChevronUp,
  Compass,
  Crosshair,
  List,
  Minus,
  Moon,
  Navigation2,
  Plus,
  Search,
  Ship,
  Sun,
  TriangleAlert,
  X,
} from "lucide-react";
import dynamic from "next/dynamic";
import Link from "next/link";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNow } from "@/app/providers";
import {
  chartBlockingHolds,
  chartCurrentPlan,
  chartHoldLabel,
  chartShipWarnings,
  chartRoster,
  chartSelectionURL,
  chartWarningAssessment,
  resolveChartDeepLink,
  chartETA,
  chartPosition,
  chartUnavailableReason,
  latestShipDecisions,
  voyageETA,
  type ChartSegment,
} from "@/lib/chart";
import {
  dataset,
  fleet,
  metadata,
  shipSource,
  useDashboard,
  useWarnings,
} from "@/lib/data";
import {
  age,
  count,
  dateTime,
  humanize,
  money,
  percent,
  quantity,
} from "@/lib/format";
import {
  nodeCoordinate,
  PORT_COORDINATES,
  routeSegments,
  type ChartPosition,
} from "@/lib/geography";
import {
  cargoEntries,
  controllerStatus,
  datasetObservation,
  observationLabel,
  shipIsLoss,
  shipLastLocation,
  shipRecordKind,
  shipOperation,
  shipStatusLabel,
  type ShipOperation,
} from "@/lib/operations";
import type {
  Account,
  Catalog,
  Clock,
  CompanyProfile,
  DatasetMeta,
  Decision,
  WarningReport,
  ShipDetail,
  PortDetail,
} from "@/lib/types";
import { useUI } from "@/lib/ui-store";
import { warningText } from "@/lib/warnings";
import { ShipLossDetails, ShipLossHistoryNotice } from "./ship-loss-details";
import { SpriteImage } from "./ui";
import styles from "./map/chart.module.css";

const FleetChart = dynamic(() => import("./map/fleet-chart"), {
  ssr: false,
  loading: () => (
    <p className={styles.loading} role="status">
      Drawing the fleet chart.
    </p>
  ),
});
const EMPTY_ACCOUNTS: Account[] = [];
const EMPTY_DECISIONS: Decision[] = [];

export interface ChartVessel {
  key: string;
  account: Account;
  company: string;
  detail: ShipDetail;
  meta: DatasetMeta;
  clock?: Clock;
  clockMeta: DatasetMeta;
  operation: ShipOperation;
  segments: ChartSegment[];
}

export interface ChartLayers {
  routes: boolean;
  labels: boolean;
  estimates: boolean;
}

export interface ChartControls {
  fitFleet: () => void;
  focusShip: (key: string) => void;
  focusPort: (id: string) => void;
  zoom: (change: number) => void;
}

function portName(catalog: Catalog | undefined, id?: string | null) {
  return (
    catalog?.ports.find((port) => port.id === id)?.name ??
    (id ? humanize(id) : "Not recorded")
  ).replaceAll(" / ", " and ");
}

function focusVisible(element: HTMLElement | null | undefined) {
  if (!element?.isConnected || !element.getClientRects().length) return false;
  element.focus({ preventScroll: true });
  return document.activeElement === element;
}

export function FleetChartPage() {
  const query = useDashboard();
  const warnings = useWarnings();
  // Polling can deliver records newer than the shared 15-second display clock.
  const now = Math.max(useNow(), query.dataUpdatedAt);
  const recordedUTC = Date.parse(query.data?.updated_at ?? "");
  const serverUTC = recordedUTC + Math.max(0, now - query.dataUpdatedAt);
  const accountID = useUI((state) => state.accountID);
  const selectedShipID = useUI((state) => state.selectedShipID);
  const selectedShipAccountID = useUI((state) => state.selectedShipAccountID);
  const selectShip = useUI((state) => state.selectShip);
  const selectedPortID = useUI((state) => state.selectedPortID);
  const selectPort = useUI((state) => state.selectPort);
  const setAccount = useUI((state) => state.setAccount);
  const theme = useUI((state) => state.theme);
  const toggleTheme = useUI((state) => state.toggleTheme);
  const accounts = query.data?.accounts ?? EMPTY_ACCOUNTS;
  const decisions = query.data?.decisions ?? EMPTY_DECISIONS;
  const scoped = useMemo(
    () =>
      accountID
        ? accounts.filter((account) => account.id === accountID)
        : accounts,
    [accounts, accountID],
  );
  const catalog = useMemo(
    () =>
      scoped
        .map((account) => dataset<Catalog>(account, "catalog"))
        .find(Boolean),
    [scoped],
  );
  const sources = useMemo(
    () =>
      scoped.flatMap((account) => {
        const company =
          dataset<CompanyProfile>(account, "profile")?.company.name ??
          account.company_id;
        const clock = dataset<Clock>(account, "clock");
        const clockMeta = metadata(account, "clock");
        return fleet(account).map((detail) => {
          return {
            key: `${account.id}:${detail.ship.id}`,
            account,
            company,
            detail,
            clock,
            clockMeta,
            meta: metadata(account, shipSource(account, detail.ship.id)),
            segments: routeSegments(detail),
          };
        });
      }),
    [scoped],
  );
  const vessels: ChartVessel[] = useMemo(
    () =>
      sources.map((row) => ({
        ...row,
        operation: shipOperation(row.account, row.detail, now),
      })),
    [sources, now],
  );
  const [layers, setLayers] = useState<ChartLayers>({
    routes: true,
    labels: true,
    estimates: true,
  });
  const [search, setSearch] = useState("");
  const [sort, setSort] = useState("name");
  const [stateFilter, setStateFilter] = useState("");
  const [rosterOpen, setRosterOpen] = useState(true);
  const [sheet, setSheet] = useState<"fleet" | "ship" | null>();
  const activeSheet =
    sheet === undefined ? (selectedShipID ? "ship" : "fleet") : sheet;
  const [positionNote, setPositionNote] = useState({ key: "", text: "" });
  const controls = useRef<ChartControls | null>(null);
  const selectionOrigin = useRef<HTMLElement | null>(null);
  const inspectorHeading = useRef<HTMLHeadingElement | null>(null);
  const deepLinkApplied = useRef(false);
  const selected = vessels.find(
    (row) =>
      row.detail.ship.id === selectedShipID &&
      row.account.id === selectedShipAccountID,
  );
  const selectedKey = selected ? selected.key : "";
  const recentDecisions = useMemo(
    () =>
      selected
        ? latestShipDecisions(
            decisions,
            selected.account.id,
            selected.detail.ship.id,
          )
        : [],
    [decisions, selected],
  );
  const uncharted = useMemo(
    () => [
      ...new Set([
        ...(catalog?.ports
          .filter((port) => !PORT_COORDINATES[port.id])
          .map((port) => port.id) ?? []),
        ...sources.flatMap(({ detail }) =>
          (detail.route_legs ?? [])
            .flatMap((leg) => [leg.from_node, leg.to_node])
            .filter((node) => !nodeCoordinate(node)),
        ),
      ]),
    ],
    [catalog, sources],
  );
  const visible = useMemo(
    () => chartRoster(vessels, search, stateFilter, sort, catalog),
    [vessels, search, stateFilter, sort, catalog],
  );
  const active = visible.filter((row) => row.detail.ship.status !== "sunk");
  const lost = visible.filter((row) =>
    shipIsLoss(row.detail.ship.status, row.detail.ship.fate),
  );
  const exits = visible.filter(
    (row) =>
      row.detail.ship.status === "sunk" &&
      !shipIsLoss(row.detail.ship.status, row.detail.ship.fate),
  );
  const historyUnavailable = scoped.some(
    (account) =>
      account.ship_losses_unavailable || account.ship_losses === undefined,
  );
  const sailing = vessels.filter(
    (row) => row.detail.ship.status === "sailing",
  ).length;
  const uncertain = vessels.filter(
    (row) =>
      row.detail.ship.status !== "sunk" &&
      row.operation.observation.state !== "current",
  ).length;

  const chooseShip = useCallback(
    (key: string) => {
      const row = vessels.find((vessel) => vessel.key === key);
      if (!row) return;
      selectionOrigin.current =
        document.activeElement instanceof HTMLElement
          ? document.activeElement
          : null;
      selectShip(row.detail.ship.id, row.account.id);
      setSheet("ship");
      requestAnimationFrame(() => {
        controls.current?.focusShip(key);
        inspectorHeading.current?.focus({ preventScroll: true });
      });
    },
    [vessels, selectShip],
  );
  const closeInspector = useCallback(() => {
    selectShip("");
    setSheet("fleet");
    requestAnimationFrame(() => {
      if (focusVisible(selectionOrigin.current)) return;
      if (
        focusVisible(
          document.querySelector<HTMLInputElement>(
            'input[aria-label="Search fleet"]',
          ),
        )
      )
        return;
      focusVisible(
        document.querySelector<HTMLButtonElement>("[data-chart-fleet-toggle]"),
      );
    });
  }, [selectShip]);
  const onReady = useCallback((value: ChartControls | null) => {
    controls.current = value;
  }, []);
  const choosePort = useCallback(
    (id: string) => {
      selectPort(id);
      setRosterOpen(true);
      setSheet("fleet");
      controls.current?.focusPort(id);
      requestAnimationFrame(() =>
        focusVisible(
          document.querySelector<HTMLElement>("[data-port-heading]"),
        ),
      );
    },
    [selectPort],
  );
  const onPositionNote = useCallback((key: string, text: string) => {
    setPositionNote((previous) =>
      previous.key === key && previous.text === text ? previous : { key, text },
    );
  }, []);

  useEffect(() => {
    if (!query.data || deepLinkApplied.current) return;
    deepLinkApplied.current = true;
    const selection = resolveChartDeepLink(accounts, window.location.search);
    if (selection) {
      if (accountID && accountID !== selection.accountID)
        setAccount(selection.accountID);
      selectShip(selection.shipID, selection.accountID);
    } else {
      window.history.replaceState(
        null,
        "",
        chartSelectionURL(window.location.href),
      );
    }
  }, [query.data, accounts, accountID, selectShip, setAccount]);
  useEffect(() => {
    if (!deepLinkApplied.current) return;
    const url = chartSelectionURL(
      window.location.href,
      selectedShipID
        ? { shipID: selectedShipID, accountID: selectedShipAccountID }
        : undefined,
    );
    window.history.replaceState(null, "", url);
  }, [selectedShipID, selectedShipAccountID]);
  useEffect(() => {
    function onKey(event: KeyboardEvent) {
      if (event.defaultPrevented || event.key !== "Escape" || !selectedShipID)
        return;
      if (
        event.target instanceof HTMLElement &&
        event.target.closest(
          "input, select, textarea, [contenteditable='true']",
        )
      )
        return;
      if (window.innerWidth <= 900 && activeSheet !== "ship") return;
      event.preventDefault();
      closeInspector();
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [selectedShipID, closeInspector, activeSheet]);
  useEffect(() => {
    if (!selectedKey) return;
    requestAnimationFrame(() =>
      document
        .querySelector<HTMLElement>(
          `[data-vessel-key="${CSS.escape(selectedKey)}"]`,
        )
        ?.scrollIntoView({ block: "nearest" }),
    );
  }, [selectedKey]);

  return (
    <main
      id="main-content"
      className={styles.room}
      data-sheet={activeSheet ?? "closed"}
      data-inspector={Boolean(selectedShipID)}
    >
      <header className={styles.topbar}>
        <div className={styles.titleGroup}>
          <Link
            href="/"
            className={styles.iconButton}
            aria-label="Back to dashboard"
          >
            <ArrowLeft size={19} aria-hidden="true" />
          </Link>
          <Compass size={24} strokeWidth={1.4} aria-hidden="true" />
          <h1>Fleet chart</h1>
        </div>
        <label className={styles.scope}>
          <span className="sr-only">Account and company</span>
          <select
            value={accountID}
            onChange={(event) => {
              setAccount(event.target.value);
              setSheet("fleet");
            }}
          >
            <option value="">All accounts and companies</option>
            {accounts.map((account) => (
              <option key={account.id} value={account.id}>
                {account.label}:{" "}
                {dataset<CompanyProfile>(account, "profile")?.company.name ??
                  "Company not recorded"}
              </option>
            ))}
            {accountID &&
              !accounts.some((account) => account.id === accountID) && (
                <option value={accountID}>Selected account unavailable</option>
              )}
          </select>
        </label>
        <div
          className={styles.layerControls}
          role="group"
          aria-label="Chart layers"
        >
          {(
            [
              ["routes", "Routes"],
              ["labels", "Port labels"],
              ["estimates", "Estimated positions"],
            ] as const
          ).map(([key, label]) => (
            <button
              key={key}
              className={styles.layerButton}
              aria-pressed={layers[key]}
              onClick={() =>
                setLayers((previous) => ({
                  ...previous,
                  [key]: !previous[key],
                }))
              }
            >
              <span className={styles.check}>
                {layers[key] && <Check size={12} aria-hidden="true" />}
              </span>
              {label}
            </button>
          ))}
        </div>
        <div className={styles.headerActions}>
          <button
            className={styles.button}
            onClick={() => controls.current?.fitFleet()}
          >
            <Crosshair size={16} aria-hidden="true" />
            Fit fleet
          </button>
          <button
            className={styles.iconButton}
            aria-label={`Switch to ${theme === "light" ? "dark" : "light"} theme`}
            onClick={toggleTheme}
          >
            {theme === "light" ? (
              <Moon size={17} aria-hidden="true" />
            ) : (
              <Sun size={17} aria-hidden="true" />
            )}
          </button>
          <div className={styles.mapZoom} role="group" aria-label="Chart zoom">
            <button
              className={styles.iconButton}
              aria-label="Zoom chart in"
              onClick={() => controls.current?.zoom(1)}
            >
              <Plus size={18} aria-hidden="true" />
            </button>
            <button
              className={styles.iconButton}
              aria-label="Zoom chart out"
              onClick={() => controls.current?.zoom(-1)}
            >
              <Minus size={18} aria-hidden="true" />
            </button>
          </div>
        </div>
      </header>
      <div className={styles.recordingStatus}>
        <span
          className={styles.statusDot}
          data-error={query.isError || uncertain > 0 || historyUnavailable}
        />
        <span role="status">
          {query.isError
            ? query.data
              ? "Updates interrupted; showing retained records"
              : "Recording service unavailable"
            : query.isPending
              ? "Connecting to recorded fleet"
              : `${count(vessels.length)} recorded vessels, ${count(sailing)} at sea`}
          {uncertain > 0 && (
            <span>
              {", "}
              {count(uncertain)} need updated records
            </span>
          )}
          {historyUnavailable && ", loss and exit history incomplete"}
        </span>
        {query.data && (
          <span className={styles.statusTime}>
            Recorded {age(query.data.updated_at, now)}
          </span>
        )}
      </div>
      <div className={styles.panels}>
        <aside
          className={styles.roster}
          data-open={rosterOpen}
          aria-label="Fleet roster"
        >
          <header className={styles.panelHeader}>
            <h2 tabIndex={-1}>
              <Ship size={17} aria-hidden="true" />
              Fleet <span>{vessels.length}</span>
            </h2>
            <button
              className={styles.iconButton}
              aria-label={
                rosterOpen ? "Collapse fleet roster" : "Expand fleet roster"
              }
              aria-expanded={rosterOpen}
              aria-controls="chart-roster"
              onClick={() => setRosterOpen(!rosterOpen)}
            >
              {rosterOpen ? (
                <ChevronUp size={17} aria-hidden="true" />
              ) : (
                <ChevronDown size={17} aria-hidden="true" />
              )}
            </button>
          </header>
          {rosterOpen && (
            <div id="chart-roster" className={styles.rosterBody}>
              <div className={styles.search}>
                <Search size={15} aria-hidden="true" />
                <input
                  type="search"
                  aria-label="Search fleet"
                  placeholder="Find a vessel, port or cargo"
                  value={search}
                  onChange={(event) => setSearch(event.target.value)}
                />
              </div>
              <div className={styles.rosterFilters}>
                <label>
                  <span className="sr-only">Sort fleet</span>
                  <select
                    value={sort}
                    onChange={(event) => setSort(event.target.value)}
                  >
                    <option value="name">Name</option>
                    <option value="arrival">Soonest arrival</option>
                    <option value="state">State</option>
                    <option value="cargo">Cargo fill</option>
                  </select>
                </label>
                <label>
                  <span className="sr-only">Filter vessel state</span>
                  <select
                    value={stateFilter}
                    onChange={(event) => setStateFilter(event.target.value)}
                  >
                    <option value="">All states</option>
                    {[
                      ...new Set(
                        [
                          ...vessels.map((row) =>
                            shipStatusLabel(
                              row.detail.ship.status,
                              row.detail.ship.fate,
                            ),
                          ),
                          stateFilter,
                        ].filter(Boolean),
                      ),
                    ]
                      .sort()
                      .map((status) => (
                        <option key={status} value={status}>
                          {status}
                        </option>
                      ))}
                  </select>
                </label>
                <label>
                  <span className="sr-only">Inspect a port</span>
                  <select
                    value={selectedPortID}
                    onChange={(event) => choosePort(event.target.value)}
                  >
                    <option value="">Inspect a port</option>
                    {catalog?.ports.map((port) => (
                      <option key={port.id} value={port.id}>
                        {port.name.replaceAll(" / ", " and ")}
                      </option>
                    ))}
                  </select>
                </label>
              </div>
              <div className={styles.rosterList}>
                {scoped.map((account) => (
                  <ShipLossHistoryNotice
                    key={account.id}
                    account={account}
                    className={styles.note}
                  />
                ))}
                {selectedPortID && (
                  <PortInspection
                    id={selectedPortID}
                    catalog={catalog}
                    accounts={scoped}
                    now={now}
                    onClose={() => selectPort("")}
                  />
                )}
                {uncharted.length > 0 && (
                  <p className={styles.note}>
                    No chart coordinates for{" "}
                    {uncharted.map((id) => portName(catalog, id)).join(", ")}.
                    Their recorded details remain available here.
                  </p>
                )}
                {query.isPending ? (
                  <p className={styles.empty}>Loading recorded vessels.</p>
                ) : !query.data ? (
                  <p className={styles.empty}>
                    The recording service is unavailable. Return to the
                    dashboard for connection details.
                  </p>
                ) : !scoped.length ? (
                  <p className={styles.empty}>
                    {accounts.length
                      ? "Selected account unavailable. Choose another scope."
                      : "No accounts recorded. Register an account with the Go service."}
                  </p>
                ) : !visible.length ? (
                  <p className={styles.empty}>
                    {vessels.length
                      ? "No vessels match these filters."
                      : "No vessels recorded in this scope."}
                  </p>
                ) : (
                  <>
                    <ul>
                      {active.map((row) => (
                        <RosterRow
                          key={row.key}
                          row={row}
                          catalog={catalog}
                          serverUTC={serverUTC}
                          selected={row.key === selectedKey}
                          onSelect={chooseShip}
                          showCompany={scoped.length > 1}
                        />
                      ))}
                    </ul>
                    {[
                      { title: "Recorded losses", rows: lost },
                      { title: "Fleet exits and absences", rows: exits },
                    ].map(
                      (group) =>
                        group.rows.length > 0 && (
                          <div key={group.title}>
                            <h3 className={styles.lossHeading}>
                              {group.title} <span>{group.rows.length}</span>
                            </h3>
                            <ul>
                              {group.rows.map((row) => (
                                <RosterRow
                                  key={row.key}
                                  row={row}
                                  catalog={catalog}
                                  serverUTC={serverUTC}
                                  selected={row.key === selectedKey}
                                  onSelect={chooseShip}
                                  showCompany={scoped.length > 1}
                                />
                              ))}
                            </ul>
                          </div>
                        ),
                    )}
                  </>
                )}
              </div>
              <p className={styles.rosterNote}>
                Select a vessel to inspect its route, recorded plan or history.
              </p>
            </div>
          )}
        </aside>
        {selectedShipID && (
          <aside className={styles.inspector} aria-label="Ship inspector">
            <header className={styles.panelHeader}>
              <div className={styles.shipTitle}>
                {selected && <SpriteImage name={selected.detail.ship.name} />}
                <div>
                  <h2 ref={inspectorHeading} tabIndex={-1}>
                    {selected?.detail.ship.name ?? "Vessel unavailable"}
                  </h2>
                  {selected && (
                    <p>
                      {humanize(selected.detail.ship.ship_class)},{" "}
                      {selected.company}
                    </p>
                  )}
                </div>
              </div>
              <button
                className={styles.iconButton}
                aria-label="Close ship inspector"
                onClick={closeInspector}
              >
                <X size={18} aria-hidden="true" />
              </button>
            </header>
            {selected ? (
              <ShipInspection
                row={selected}
                catalog={
                  dataset<Catalog>(selected.account, "catalog") ?? catalog
                }
                decisions={recentDecisions}
                now={now}
                serverUTC={serverUTC}
                positionNote={
                  positionNote.key === selected.key ? positionNote.text : ""
                }
                estimates={layers.estimates}
                warningReport={warnings.data}
                warningQueryStatus={
                  warnings.isError
                    ? "error"
                    : warnings.isPending
                      ? "pending"
                      : "success"
                }
              />
            ) : (
              <p className={styles.empty}>
                This selection is retained, but the vessel is not present in the
                current scope or latest recording. Select another vessel or
                change account scope.
              </p>
            )}
          </aside>
        )}
      </div>
      <FleetChart
        vessels={vessels}
        catalog={catalog}
        snapshotUTC={query.data?.updated_at}
        snapshotReceivedAt={query.dataUpdatedAt}
        selectedKey={selectedKey}
        inspectorOpen={Boolean(selectedShipID)}
        selectedPortID={selectedPortID}
        onSelectPort={choosePort}
        layers={layers}
        rosterOpen={rosterOpen}
        onSelect={chooseShip}
        onReady={onReady}
        onPositionNote={onPositionNote}
      />
      <footer className={styles.legend} aria-label="Chart legend">
        <details>
          <summary>Chart key</summary>
          <div className={styles.keyItems}>
            <span>
              <span className={styles.legendHull} /> Recorded port
            </span>
            <span>
              <span className={`${styles.legendHull} ${styles.hollow}`} />{" "}
              Estimated at sea
            </span>
            <span>
              <span className={styles.legendLine} /> Scheduled route (schematic)
            </span>
            <span className={styles.warning}>
              <Anchor size={13} aria-hidden="true" /> Anchored
            </span>
            <span className={styles.warning}>
              <span className={`${styles.legendHull} ${styles.hollow}`} />{" "}
              Frozen estimate
            </span>
            <span className={styles.attention}>
              <TriangleAlert size={13} aria-hidden="true" /> Hold or provisions
              risk
            </span>
            <span className={styles.attention}>
              <X size={13} aria-hidden="true" /> Wreck: observed port only
            </span>
            <span>
              <span className={styles.legendHub} /> Hub port
            </span>
            <span>
              <span className={styles.legendYard} /> Shipyard port
            </span>
            <small>
              Route lines, sea positions and ETAs are estimates. A wake is not a
              measured track; a scheduled arrival is not confirmation. Wreck
              symbols do not show confirmed loss locations.
            </small>
          </div>
        </details>
      </footer>
      <nav className={styles.sheetNav} aria-label="Chart panels">
        <button
          data-chart-fleet-toggle
          aria-expanded={activeSheet === "fleet"}
          onClick={() => {
            setRosterOpen(true);
            setSheet(activeSheet === "fleet" ? null : "fleet");
            if (activeSheet !== "fleet")
              requestAnimationFrame(() =>
                focusVisible(
                  document.querySelector<HTMLInputElement>(
                    'input[aria-label="Search fleet"]',
                  ),
                ),
              );
          }}
        >
          <List size={18} aria-hidden="true" />
          Fleet ({vessels.length})
          {activeSheet === "fleet" ? (
            <ChevronDown size={16} aria-hidden="true" />
          ) : (
            <ChevronUp size={16} aria-hidden="true" />
          )}
        </button>
        {selectedShipID && (
          <button
            aria-expanded={activeSheet === "ship"}
            onClick={() => {
              setSheet(activeSheet === "ship" ? null : "ship");
              if (activeSheet !== "ship")
                requestAnimationFrame(() =>
                  focusVisible(inspectorHeading.current),
                );
            }}
          >
            <Ship size={18} aria-hidden="true" />
            {selected?.detail.ship.name ?? "Selected vessel"}
            {activeSheet === "ship" ? (
              <ChevronDown size={16} aria-hidden="true" />
            ) : (
              <ChevronUp size={16} aria-hidden="true" />
            )}
          </button>
        )}
      </nav>
    </main>
  );
}

function RosterRow({
  row,
  catalog,
  serverUTC,
  selected,
  onSelect,
  showCompany,
}: {
  row: ChartVessel;
  catalog?: Catalog;
  serverUTC: number;
  selected: boolean;
  onSelect: (key: string) => void;
  showCompany: boolean;
}) {
  const { detail, operation } = row;
  const { ship } = detail;
  const sailing = ship.status === "sailing";
  const gone = ship.status === "sunk";
  const holds = operation.holdsCurrent
    ? chartBlockingHolds(operation.holds, ship.status)
    : [];
  const attention = !gone && (operation.starving || holds.length > 0);
  const capacity = detail.effective_hold_capacity_tons;
  const cargoTons = detail.cargo_used_tons;
  const cargoKnown =
    typeof cargoTons === "number" &&
    Number.isFinite(cargoTons) &&
    typeof capacity === "number" &&
    Number.isFinite(capacity);
  const fill =
    cargoKnown && capacity > 0
      ? Math.max(0, Math.min(100, (cargoTons / capacity) * 100))
      : undefined;
  const cargoSummary = cargoKnown
    ? `${count(cargoTons)} of ${quantity(capacity, "t")}`
    : "Cargo use not recorded";
  const destination = sailing
    ? `to ${portName(catalog, detail.route_legs?.at(-1)?.to_node)}`
    : gone
      ? shipLastLocation(detail, catalog)
      : portName(catalog, ship.current_port_id);
  const state = gone
    ? shipStatusLabel(ship.status, ship.fate)
    : ship.status.startsWith("anchored")
      ? "Anchored"
      : operation.activity;
  const eta = chartETA(
    detail,
    row.clock,
    row.clockMeta,
    row.meta,
    serverUTC,
  ).replace(/^in /, "");
  return (
    <li>
      <button
        className={styles.vesselRow}
        data-vessel-key={row.key}
        aria-pressed={selected}
        data-state={attention ? "attention" : ship.status}
        title={showCompany ? `${row.account.label}: ${row.company}` : undefined}
        onClick={() => onSelect(row.key)}
      >
        <span className={styles.vesselRowHeading}>
          <SpriteImage name={ship.name} />
          <strong>{ship.name}</strong>
          {attention && (
            <small
              className={styles.attention}
              title={holds.map((hold) => hold.reason).join(". ")}
            >
              {operation.starving
                ? "Provisions at risk"
                : chartHoldLabel(holds[0].reason)}
            </small>
          )}
          {!gone && !operation.observation.current && (
            <small className={styles.attention}>
              {observationLabel(operation.observation.state)}
            </small>
          )}
        </span>
        <span className={styles.vesselRoute}>
          <span>{state}</span>
          <span className={styles.rowDestination} title={destination}>
            {destination}
          </span>
          {sailing && <small className={styles.rowETA}>{eta}</small>}
        </span>
        {gone ? (
          <span
            className={styles.cargoSummary}
            aria-label="Last observed cargo"
          >
            {operation.cargo}
          </span>
        ) : (
          <span
            className={styles.cargoSummary}
            aria-label={`Cargo: ${cargoSummary}`}
          >
            <span className={styles.cargoTrack} aria-hidden="true">
              {fill !== undefined && <span style={{ width: `${fill}%` }} />}
            </span>
            <span>{cargoSummary}</span>
          </span>
        )}
      </button>
    </li>
  );
}

function ShipInspection({
  row,
  catalog,
  decisions,
  now,
  serverUTC,
  positionNote,
  estimates,
  warningReport,
  warningQueryStatus,
}: {
  row: ChartVessel;
  catalog?: Catalog;
  decisions: Decision[];
  now: number;
  serverUTC: number;
  positionNote: string;
  estimates: boolean;
  warningReport?: WarningReport;
  warningQueryStatus: "pending" | "error" | "success";
}) {
  const { detail, operation } = row;
  const { ship } = detail;
  const sailing = ship.status === "sailing";
  const gone = ship.status === "sunk";
  const position: ChartPosition | undefined = chartPosition(
    detail,
    row.meta,
    row.clock,
    row.clockMeta,
    serverUTC,
  );
  const controller = controllerStatus(row.account, now);
  const report = warningReport?.accounts.find(
    (account) => account.account_id === row.account.id,
  );
  const currentPlan = gone
    ? undefined
    : chartCurrentPlan(row.account, ship.id, decisions);
  const holds = operation.holdsCurrent
    ? chartBlockingHolds(operation.holds, ship.status)
    : [];
  const shipWarnings = chartShipWarnings(report?.warnings ?? [], detail);
  const warningAssessment = chartWarningAssessment(
    warningReport,
    row.account.id,
    warningQueryStatus,
    operation.holdsCurrent,
  );
  const needsAttention =
    holds.length > 0 || operation.starving || shipWarnings.length > 0;
  const positionSummary = !sailing
    ? "Recorded port"
    : !estimates
      ? "Estimates hidden"
      : row.clock?.paused
        ? "Estimate paused"
        : positionNote.includes("frozen")
          ? "Estimate frozen"
          : position?.kind === "awaiting"
            ? "Awaiting arrival record"
            : position?.kind === "estimated"
              ? "Estimated at sea"
              : "Position unavailable";
  const manifest = cargoEntries(detail.cargo_manifest);
  const eta = voyageETA(detail);
  const locationLabel = gone
    ? shipLastLocation(detail, catalog)
    : ship.current_port_id
      ? portName(catalog, ship.current_port_id)
      : "Position not recorded";
  return (
    <div className={styles.inspectorBody}>
      <section className={styles.journey} aria-label="Vessel location">
        <div className={styles.shipState} data-state={ship.status}>
          {sailing ? (
            <Navigation2 size={17} aria-hidden="true" />
          ) : (
            <Anchor size={17} aria-hidden="true" />
          )}
          {shipStatusLabel(ship.status, ship.fate)}
        </div>
        <h3>
          {sailing
            ? `${portName(catalog, detail.route_legs?.[0]?.from_node)} to ${portName(catalog, detail.route_legs?.at(-1)?.to_node)}`
            : locationLabel}
        </h3>
        <p
          className={styles.positionNote}
          title={
            positionNote ||
            position?.label ||
            chartUnavailableReason(detail, row.clock)
          }
        >
          {gone
            ? (position?.label ?? chartUnavailableReason(detail, row.clock))
            : `${positionSummary}, recorded ${age(row.meta.updated_at, now)}`}
        </p>
        {sailing && (
          <div className={styles.arrival}>
            <strong>
              {chartETA(detail, row.clock, row.clockMeta, row.meta, serverUTC)}
            </strong>
            <span>
              {eta === undefined
                ? "Full voyage arrival time is unavailable"
                : `Scheduled ${dateTime(new Date(eta).toISOString())}`}
            </span>
          </div>
        )}
        {gone && (
          <p className={styles.attention}>
            {shipIsLoss(ship.status, ship.fate)
              ? "Retained outside the active fleet. A wreck symbol marks only an observed port, not a confirmed loss location. At-sea observations remain unplaced."
              : shipRecordKind(ship.status, ship.fate) === "absence"
                ? "Retained absence record. Being absent from the fleet list does not confirm the ship's fate."
                : "Retained fleet exit record. The last afloat observation does not confirm the exit location."}
          </p>
        )}
        {!gone && !row.account.enabled && (
          <p className={styles.attention}>
            Automation disabled for this account.
          </p>
        )}
        {!gone && controller.mode === "observe" && (
          <p className={styles.note}>
            Observation only. The controller is not executing plans.
          </p>
        )}
      </section>
      <ShipLossDetails
        detail={detail}
        catalog={catalog}
        historyUnavailable={
          row.account.ship_losses_unavailable ||
          row.account.ship_losses === undefined
        }
        className={styles.inspectionSection}
        factsClassName={styles.facts}
        noteClassName={styles.note}
      />
      {!gone && (
        <>
          <section className={styles.inspectionSection}>
            <div className={styles.planHeading}>
              <h3>
                {currentPlan?.source === "decision"
                  ? "Latest decision"
                  : "Current plan"}
              </h3>
              {currentPlan && (
                <time
                  dateTime={currentPlan.updatedAt}
                  title={dateTime(currentPlan.updatedAt)}
                >
                  {age(currentPlan.updatedAt, now)}
                </time>
              )}
            </div>
            {currentPlan ? (
              <>
                <p className={styles.planLane}>
                  <strong title={currentPlan.contract_id}>
                    {currentPlan.rejected ? "Rejected: " : ""}
                    {humanize(currentPlan.kind)}
                  </strong>
                  {currentPlan.commodity
                    ? `: ${currentPlan.quantity ? `${count(currentPlan.quantity)} t ` : ""}${humanize(currentPlan.commodity)}`
                    : ""}
                </p>
                {currentPlan.destination ? (
                  <p className={styles.note}>
                    {currentPlan.origin
                      ? `${portName(catalog, currentPlan.origin)} to `
                      : "To "}
                    {portName(catalog, currentPlan.destination)}
                  </p>
                ) : currentPlan.origin ? (
                  <p className={styles.note}>
                    At {portName(catalog, currentPlan.origin)}
                  </p>
                ) : null}
                <p className={styles.planValue}>
                  {currentPlan.expectedNetPence === undefined
                    ? "Value not recorded"
                    : `${money(currentPlan.expectedNetPence)} estimated net`}
                </p>
              </>
            ) : (
              <p className={styles.note}>
                {row.account.missions_unavailable
                  ? "Plan unavailable."
                  : "Plan not recorded."}
              </p>
            )}
          </section>
          {(needsAttention || warningAssessment.status !== "assessed") && (
            <section className={styles.inspectionSection}>
              <h3>{needsAttention ? "Needs attention" : "Ship warnings"}</h3>
              {warningAssessment.message && (
                <p className={styles.note}>{warningAssessment.message}</p>
              )}
              {operation.starving && (
                <p className={styles.warning}>Provisions at risk</p>
              )}
              {holds.map((hold, index) => (
                <p key={index} className={styles.warning} title={hold.reason}>
                  {chartHoldLabel(hold.reason)}
                </p>
              ))}
              {shipWarnings.map((warning) => (
                <p
                  key={warning.code}
                  className={styles.warning}
                  title={warningText(warning)}
                >
                  {warning.code === "upkeep_seizure"
                    ? "Upkeep seizure recorded"
                    : "Ship held"}
                </p>
              ))}
            </section>
          )}
          <section className={styles.inspectionSection}>
            <h3>Cargo and stores</h3>
            <dl className={styles.facts}>
              <div>
                <dt>Cargo hold</dt>
                <dd>
                  {detail.cargo_used_tons !== null &&
                  detail.effective_hold_capacity_tons !== null &&
                  Number.isFinite(detail.cargo_used_tons) &&
                  Number.isFinite(detail.effective_hold_capacity_tons)
                    ? `${count(detail.cargo_used_tons)} of ${quantity(detail.effective_hold_capacity_tons, "t")}`
                    : "Not recorded"}
                </dd>
              </div>
              <div>
                <dt>Provisions</dt>
                <dd>{quantity(detail.stores?.provisions_tons, "t")}</dd>
              </div>
              <div>
                <dt>Sea days remaining</dt>
                <dd>{count(detail.stores?.sea_days_remaining)}</dd>
              </div>
              <div>
                <dt>Cannons</dt>
                <dd>{count(ship.cannons_count)}</dd>
              </div>
            </dl>
            {manifest === undefined ? (
              <p className={styles.note}>Cargo manifest not recorded.</p>
            ) : manifest.length ? (
              <ul className={styles.manifest}>
                {manifest.map(([commodity, tons]) => (
                  <li key={commodity}>
                    <span>
                      {catalog?.commodities.find(
                        (item) => item.id === commodity,
                      )?.name ?? humanize(commodity)}
                    </span>
                    <strong>{quantity(tons, "t")}</strong>
                  </li>
                ))}
              </ul>
            ) : (
              <p className={styles.note}>
                No trade cargo in the recorded manifest.
              </p>
            )}
          </section>
          <section className={styles.inspectionSection}>
            <h3>Recorded route</h3>
            {detail.route_legs?.length ? (
              <ol className={styles.routeLegs}>
                {detail.route_legs.map((leg, index) => {
                  const current = sailing && index === ship.current_leg_idx;
                  const voyageLeg = detail.voyage?.legs?.find(
                    (item) => item.leg_idx === index,
                  );
                  const arrival = current
                    ? ship.leg_arrives_at
                    : voyageLeg?.estimated_arrival_at;
                  return (
                    <li
                      key={`${index}:${leg.from_node}`}
                      aria-current={current ? "step" : undefined}
                    >
                      <strong>
                        {portName(catalog, leg.from_node)} to{" "}
                        {portName(catalog, leg.to_node)}
                      </strong>
                      <span>
                        {count(leg.distance_nm)} nm, {humanize(leg.edge_class)}
                        {current
                          ? ", current leg"
                          : sailing &&
                              ship.current_leg_idx !== null &&
                              index < ship.current_leg_idx
                            ? ", previous leg"
                            : ""}
                      </span>
                      {current && (
                        <small>Departs {dateTime(ship.leg_departs_at)}</small>
                      )}
                      <small>Scheduled arrival {dateTime(arrival)}</small>
                    </li>
                  );
                })}
              </ol>
            ) : (
              <p className={styles.note}>No route legs recorded.</p>
            )}
          </section>
        </>
      )}
      <section className={styles.inspectionSection}>
        <h3>Recording</h3>
        <dl className={styles.facts}>
          <div>
            <dt>Account</dt>
            <dd>{row.account.label}</dd>
          </div>
          <div>
            <dt>{gone ? "Fleet data observed" : "Ship observed"}</dt>
            <dd>{dateTime(row.meta.updated_at)}</dd>
          </div>
          <div>
            <dt>Upstream as of</dt>
            <dd>{dateTime(row.meta.upstream_as_of)}</dd>
          </div>
          <div>
            <dt>Clock observed</dt>
            <dd>{dateTime(row.clockMeta.updated_at)}</dd>
          </div>
        </dl>
        {row.meta.error && (
          <p className={styles.attention}>Recording failed: {row.meta.error}</p>
        )}
        <p className={styles.note}>Ship {ship.id}</p>
      </section>
    </div>
  );
}

function PortInspection({
  id,
  catalog,
  accounts,
  now,
  onClose,
}: {
  id: string;
  catalog?: Catalog;
  accounts: Account[];
  now: number;
  onClose: () => void;
}) {
  const port = catalog?.ports.find((item) => item.id === id);
  return (
    <section className={styles.portInspection} aria-label="Recorded harbour">
      <div className={styles.planHeading}>
        <h3 tabIndex={-1} data-port-heading>
          {port?.name ?? humanize(id)}
        </h3>
        <button
          className={styles.iconButton}
          aria-label="Close port details"
          onClick={onClose}
        >
          <X size={15} aria-hidden="true" />
        </button>
      </div>
      <p className={styles.note}>
        {port?.is_hub ? "Hub port. " : ""}
        {port?.has_shipyard ? "Shipyard. " : ""}
        {port
          ? `${humanize(port.banking_region)}, phase ${port.phase}`
          : "Port absent from the current catalog."}
      </p>
      {!PORT_COORDINATES[id] && (
        <p className={styles.attention}>No chart coordinates for this port.</p>
      )}
      {accounts.map((account) => {
        const name = `ports/${id}`;
        const detail = dataset<PortDetail>(account, name);
        const meta = metadata(account, name);
        return (
          <div key={account.id} className={styles.portRecording}>
            <strong>{account.label}</strong>
            {detail ? (
              <>
                <dl className={styles.facts}>
                  <div>
                    <dt>Occupied berths</dt>
                    <dd>
                      {count(detail.occupied_berths)} of{" "}
                      {count(detail.effective_berths)}
                    </dd>
                  </div>
                  <div>
                    <dt>Queued ships</dt>
                    <dd>{count(detail.queued_ships)}</dd>
                  </div>
                  <div>
                    <dt>Health</dt>
                    <dd>{percent(detail.health_score_bps)}</dd>
                  </div>
                  <div>
                    <dt>Berth dues per day</dt>
                    <dd>{money(detail.berth_dues_per_day_pence)}</dd>
                  </div>
                  <div>
                    <dt>Visibility</dt>
                    <dd>
                      {detail.visibility
                        ? humanize(detail.visibility)
                        : "Not recorded"}
                    </dd>
                  </div>
                </dl>
                <small className={styles.note}>
                  {observationLabel(
                    datasetObservation(account, name, now).state,
                  )}
                  . Observed {dateTime(meta.updated_at)}. Upstream{" "}
                  {dateTime(meta.upstream_as_of)}.
                </small>
                {meta.error && (
                  <p className={styles.attention}>
                    Recording failed: {meta.error}
                  </p>
                )}
              </>
            ) : (
              <p className={styles.note}>
                Harbour conditions not recorded for this account.
              </p>
            )}
          </div>
        );
      })}
    </section>
  );
}
