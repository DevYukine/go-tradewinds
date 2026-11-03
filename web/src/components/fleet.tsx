"use client";

import * as Dialog from "@radix-ui/react-dialog";
import { Compass, Ship, X } from "lucide-react";
import Link from "next/link";
import {
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";
import { useNow } from "@/app/providers";
import { dataset, fleet } from "@/lib/data";
import { count, dateTime, humanize, quantity } from "@/lib/format";
import {
  catalogPlaceName,
  shipLastLocation,
  shipStatusLabel,
} from "@/lib/operations";
import { shipSummary, type ShipSummary } from "@/lib/ship-summary";
import type {
  Account,
  Catalog,
  CompanyProfile,
  DashboardData,
  ShipDetail,
} from "@/lib/types";
import { useUI } from "@/lib/ui-store";
import { ShipInspector } from "./ship-inspector";
import { OperationsOverview } from "./operations-overview";
import { ShipLossHistoryNotice } from "./ship-loss-details";
import {
  DataTable,
  DatasetStatus,
  EmptyState,
  SpriteImage,
  type Column,
} from "./ui";
import styles from "./fleet.module.css";

interface FleetRow {
  account: Account;
  detail: ShipDetail;
  company: string;
  className: string;
  summary: ShipSummary;
}

const WIDE_INSPECTOR = "(min-width: 1280px)";
function subscribeWidth(listener: () => void) {
  const query = window.matchMedia(WIDE_INSPECTOR);
  query.addEventListener("change", listener);
  return () => query.removeEventListener("change", listener);
}
function wideSnapshot() {
  return window.matchMedia(WIDE_INSPECTOR).matches;
}
function serverWidth() {
  return false;
}
function fleetRowKey(row: FleetRow) {
  return `${row.account.id}:${row.detail.ship.id}`;
}
function destination(detail: ShipDetail) {
  return detail.ship.status === "sailing"
    ? detail.route_legs?.at(-1)?.to_node
    : undefined;
}

export function FleetPanel({
  accounts,
  catalog,
  dashboard,
  compact = false,
  receivedAt = 0,
}: {
  accounts: Account[];
  catalog?: Catalog;
  dashboard: DashboardData;
  compact?: boolean;
  receivedAt?: number;
}) {
  const now = Math.max(useNow(), receivedAt);
  const [statusFilter, setStatusFilter] = useState("");
  const [portFilter, setPortFilter] = useState("");
  const selectedShipID = useUI((state) => state.selectedShipID);
  const selectedShipAccountID = useUI((state) => state.selectedShipAccountID);
  const [compactOpen, setCompactOpen] = useState(false);
  const panelRoot = useRef<HTMLDivElement | null>(null);
  const selectShip = useUI((state) => state.selectShip);
  const wide = useSyncExternalStore(subscribeWidth, wideSnapshot, serverWidth);
  const ledgerHeading = useRef<HTMLHeadingElement | null>(null);
  const returnFocus = useRef<HTMLElement | null>(null);
  const headingID = useId();
  const restoreFocus = () => {
    const target = returnFocus.current?.isConnected
      ? returnFocus.current
      : (ledgerHeading.current ?? panelRoot.current);
    returnFocus.current = null;
    target?.focus({ preventScroll: true });
  };
  const clearSelection = () => {
    selectShip("");
    restoreFocus();
  };
  const ships = useMemo(
    () =>
      accounts.flatMap((account) => {
        const observedCatalog = dataset<Catalog>(account, "catalog") ?? catalog;
        const company =
          dataset<CompanyProfile>(account, "profile")?.company.name ??
          account.company_id;
        return fleet(account).map((detail) => ({
          account,
          detail,
          company,
          className:
            observedCatalog?.ship_classes.find(
              (item) => item.id === detail.ship.ship_class,
            )?.name ?? humanize(detail.ship.ship_class),
        }));
      }),
    [accounts, catalog],
  );
  const rows = useMemo<FleetRow[]>(
    () =>
      compact
        ? []
        : ships.map((row) => ({
            ...row,
            summary: shipSummary(
              row.account,
              row.detail,
              dashboard.decisions,
              now,
              catalog,
            ),
          })),
    [compact, ships, dashboard.decisions, now, catalog],
  );
  const selected = ships.find(
    (row) =>
      row.account.id === selectedShipAccountID &&
      row.detail.ship.id === selectedShipID,
  );
  const selectionInScope = Boolean(
    selectedShipID &&
    accounts.some((account) => account.id === selectedShipAccountID),
  );
  if (compactOpen && (!compact || !selectionInScope)) {
    setCompactOpen(false);
  }
  const selectedCatalog = selected
    ? (dataset<Catalog>(selected.account, "catalog") ?? catalog)
    : catalog;
  useEffect(() => {
    if (compact || !wide || !selectionInScope) return;
    const active = document.activeElement;
    if (
      active instanceof HTMLElement &&
      active !== document.body &&
      !active.closest("#ship-inspector")
    ) {
      returnFocus.current = active;
    }
  }, [compact, wide, selectionInScope, selectedShipID, selectedShipAccountID]);
  const statuses = useMemo(
    () =>
      [
        ...new Set([
          ...rows.map((row) =>
            shipStatusLabel(row.detail.ship.status, row.detail.ship.fate),
          ),
          ...(statusFilter && statusFilter !== "starving"
            ? [statusFilter]
            : []),
        ]),
      ].sort(),
    [rows, statusFilter],
  );
  const ports = useMemo(
    () =>
      [
        ...new Set(
          [
            ...rows.flatMap((row) =>
              row.detail.ship.status === "sunk"
                ? []
                : [row.detail.ship.current_port_id, destination(row.detail)],
            ),
            portFilter,
          ].filter((id): id is string => Boolean(id)),
        ),
      ].sort(),
    [rows, portFilter],
  );
  const filtered = useMemo(
    () =>
      rows.filter(({ detail }) => {
        const matchesStatus =
          !statusFilter ||
          (statusFilter === "starving"
            ? detail.ship.status !== "sunk" &&
              detail.voyage?.legs?.some((leg) => leg.starving)
            : shipStatusLabel(detail.ship.status, detail.ship.fate) ===
              statusFilter);
        const matchesPort =
          !portFilter ||
          (detail.ship.status !== "sunk" &&
            (detail.ship.current_port_id === portFilter ||
              destination(detail) === portFilter));
        return matchesStatus && matchesPort;
      }),
    [rows, statusFilter, portFilter],
  );
  const columns = useMemo<Column<FleetRow>[]>(
    () => [
      {
        label: "Ship",
        value: ({ detail }) => detail.ship.name,
        className: styles.shipCell,
        render: (row) => (
          <button
            className={styles.shipButton}
            aria-pressed={
              row.detail.ship.id === selectedShipID &&
              row.account.id === selectedShipAccountID
            }
            aria-label={`Inspect ${row.detail.ship.name}, ${row.account.label}, ${row.company}`}
            onClick={(event) => {
              returnFocus.current = event.currentTarget;
              selectShip(row.detail.ship.id, row.account.id);
            }}
          >
            <SpriteImage name={row.detail.ship.name} />
            <span>
              {row.detail.ship.name}
              <span className={styles.cellSubtext}>{row.className}</span>
            </span>
          </button>
        ),
      },
      {
        label: "Cargo",
        value: ({ detail }) =>
          detail.ship.status === "sunk"
            ? undefined
            : (detail.cargo_used_tons ?? undefined),
        render: ({ summary }) => (
          <span className={styles.loadSummary}>
            {summary.cargo}
            {!summary.gone && (
              <span className={styles.cellSubtext}>
                {summary.crew === undefined
                  ? "Crew requirement not recorded"
                  : `Crew required ${count(summary.crew)}`}
                <br />
                {summary.passengers.text}
              </span>
            )}
          </span>
        ),
      },
      {
        label: "Observed activity",
        value: ({ detail }) =>
          `${shipStatusLabel(detail.ship.status, detail.ship.fate)}${detail.ship.status !== "sunk" && detail.voyage?.legs?.some((leg) => leg.starving) ? " starving" : ""}`,
        render: ({ summary }) => (
          <span
            className={
              summary.gone && summary.why.attention
                ? styles.attention
                : undefined
            }
          >
            {summary.action}
          </span>
        ),
      },
      {
        label: "Why",
        value: ({ summary }) =>
          summary.why.text
            ? `${summary.why.label}: ${summary.why.text}`
            : undefined,
        render: ({ summary }) =>
          summary.why.text ? (
            <span
              className={summary.why.attention ? styles.attention : undefined}
            >
              {summary.why.label}: {summary.why.text}
            </span>
          ) : null,
      },
      {
        label: "Port / destination",
        value: ({ detail, summary }) =>
          detail.ship.status === "sunk"
            ? shipLastLocation(detail, summary.catalog)
            : catalogPlaceName(
                summary.catalog,
                destination(detail) ?? detail.ship.current_port_id,
              ),
        className: styles.location,
        render: ({ detail, summary }) =>
          detail.ship.status === "sunk"
            ? shipLastLocation(detail, summary.catalog)
            : detail.ship.status === "sailing"
              ? destination(detail)
                ? `To ${catalogPlaceName(summary.catalog, destination(detail))}`
                : "Destination not recorded"
              : catalogPlaceName(summary.catalog, detail.ship.current_port_id),
      },
      {
        label: "Stores",
        value: ({ detail }) =>
          detail.ship.status === "sunk"
            ? detail.loss?.stores?.sea_days_remaining
            : detail.stores?.sea_days_remaining,
        render: ({ detail }) =>
          detail.ship.status === "sunk"
            ? detail.loss?.stores
              ? `Last afloat: ${quantity(detail.loss.stores.sea_days_remaining, "sea days")}`
              : "Last afloat stores not recorded"
            : detail.stores
              ? quantity(detail.stores.sea_days_remaining, "sea days")
              : "Not recorded",
        className: styles.numeric,
      },
      {
        label: "Company and account",
        value: ({ account, company }) => `${company} ${account.label}`,
        render: ({ account, company }) => (
          <span>
            {company || "Not recorded"}
            <span className={styles.cellSubtext}>{account.label}</span>
          </span>
        ),
      },
      {
        label: "Scheduled arrival",
        value: ({ detail }) => {
          if (detail.ship.status !== "sailing") return undefined;
          const timestamp = Date.parse(
            detail.voyage?.estimated_arrival_at ?? "",
          );
          return Number.isFinite(timestamp) ? timestamp : undefined;
        },
        render: ({ detail }) =>
          detail.ship.status === "sailing"
            ? dateTime(detail.voyage?.estimated_arrival_at)
            : "No active voyage",
      },
    ],
    [selectedShipID, selectedShipAccountID, selectShip],
  );
  const description = selected
    ? `${selected.company}, configured account ${selected.account.label}`
    : "The selected ship is not present in the latest recorded fleet for this scope.";
  const inspector = selected ? (
    <ShipInspector
      key={`${selected.account.id}:${selected.detail.ship.id}`}
      account={selected.account}
      detail={selected.detail}
      catalog={selectedCatalog}
      decisions={dashboard.decisions}
      receivedAt={receivedAt}
      summary={
        rows.find(
          (row) =>
            row.account.id === selected.account.id &&
            row.detail.ship.id === selected.detail.ship.id,
        )?.summary
      }
    />
  ) : (
    <EmptyState
      icon={Ship}
      title="Selected ship is unavailable"
      description="The selection is retained. Choose another vessel or wait for a complete fleet recording."
    />
  );

  return (
    <div
      ref={panelRoot}
      tabIndex={-1}
      className={`${styles.workspace} ${compact ? styles.compact : ""}`}
    >
      {!compact &&
        accounts.map((account) => (
          <ShipLossHistoryNotice
            key={account.id}
            account={account}
            className={styles.note}
          />
        ))}
      {compact && selectedShipID && (
        <section className={styles.sectionHeading} aria-label="Ship selection">
          <p>
            {selectionInScope ? (
              <>
                Selected ship:{" "}
                <strong>
                  {selected?.detail.ship.name ?? "Ship unavailable"}
                </strong>
                ,{" "}
                {
                  accounts.find(
                    (account) => account.id === selectedShipAccountID,
                  )?.label
                }
                .
              </>
            ) : (
              "Ship selection is outside this account scope."
            )}
          </p>
          <div className={styles.ledgerActions}>
            {selectionInScope && (
              <button
                type="button"
                className={styles.button}
                onClick={(event) => {
                  returnFocus.current = event.currentTarget;
                  setCompactOpen(true);
                }}
              >
                Inspect selected ship
              </button>
            )}
            <button
              type="button"
              className={styles.button}
              onClick={clearSelection}
            >
              Clear ship selection
            </button>
          </div>
        </section>
      )}
      {compact && (
        <OperationsOverview
          accounts={accounts}
          dashboard={dashboard}
          catalog={catalog}
          receivedAt={receivedAt}
          inspectorOpen={compactOpen}
          onInspect={(trigger) => {
            returnFocus.current = trigger;
            setCompactOpen(true);
          }}
        />
      )}
      <div
        className={`${styles.fleetLayout} ${!compact && wide && selectionInScope ? styles.withInspector : ""}`}
      >
        {compact && (
          <section
            className={`${styles.sectionHeading} ${styles.chartEntry}`}
            aria-label="Fleet chart"
          >
            <div>
              <h2>Fleet chart</h2>
              <p>
                Open the focused chart to follow recorded routes and inspect
                ships, voyage legs and position estimates.
              </p>
            </div>
            <Link className={styles.button} href="/chart">
              <Compass size={17} strokeWidth={1.7} aria-hidden="true" />
              Fleet chart
            </Link>
          </section>
        )}
        {!compact && (
          <section
            className={styles.ledger}
            id="fleet-ledger"
            aria-labelledby={headingID}
            tabIndex={-1}
          >
            <header className={styles.sectionHeading}>
              <div>
                <h2
                  id={headingID}
                  ref={ledgerHeading}
                  tabIndex={-1}
                  className={styles.fleetHeading}
                >
                  <Ship size={17} strokeWidth={1.7} aria-hidden="true" />
                  Vessel ledger
                </h2>
                <p>
                  Recorded vessels, cargo and stores. Select a vessel to inspect
                  it.
                </p>
              </div>
              <div className={styles.ledgerActions}>
                <Link className={styles.button} href="/chart">
                  <Compass size={17} strokeWidth={1.7} aria-hidden="true" />
                  Fleet chart
                </Link>
                {selectionInScope && (
                  <button className={styles.button} onClick={clearSelection}>
                    Clear ship selection
                  </button>
                )}
              </div>
            </header>
            <div className={styles.observations}>
              {accounts.map((account) => (
                <div className={styles.observation} key={account.id}>
                  <strong>{account.label}</strong>
                  <DatasetStatus
                    account={account}
                    name="ships"
                    receivedAt={receivedAt}
                  />
                </div>
              ))}
            </div>
            <div className={styles.filters}>
              <label className={styles.filterField}>
                Status, fate and warnings
                <select
                  value={statusFilter}
                  onChange={(event) => setStatusFilter(event.target.value)}
                >
                  <option value="">All recorded states</option>
                  {statuses.map((status) => (
                    <option key={status} value={status}>
                      {status}
                    </option>
                  ))}
                  <option value="starving">
                    Starvation in voyage estimate
                  </option>
                </select>
              </label>
              <label className={styles.filterField}>
                Current or destination port
                <select
                  value={portFilter}
                  onChange={(event) => setPortFilter(event.target.value)}
                >
                  <option value="">All ports</option>
                  {ports.map((portID) => (
                    <option key={portID} value={portID}>
                      {catalog?.ports.find((port) => port.id === portID)
                        ?.name ?? humanize(portID)}
                    </option>
                  ))}
                </select>
              </label>
              {(statusFilter || portFilter) && (
                <button
                  className={styles.button}
                  onClick={() => {
                    setStatusFilter("");
                    setPortFilter("");
                  }}
                >
                  Clear fleet filters
                </button>
              )}
            </div>
            <p className={styles.resultCount}>
              {count(filtered.length)} of {count(rows.length)} recorded ships
              match the fleet filters.
            </p>
            <DataTable
              columns={columns}
              data={filtered}
              rowKey={fleetRowKey}
              label="Vessel ledger"
              empty={
                rows.length
                  ? "No ships match these fleet filters"
                  : "No ships have been recorded"
              }
            />
          </section>
        )}
        {!compact && wide && selectionInScope && (
          <aside
            className={styles.inspector}
            id="ship-inspector"
            aria-label="Selected ship inspector"
          >
            <header className={styles.inspectorHeading}>
              <div>
                <h2 className="sr-only">
                  {selected?.detail.ship.name ?? "Selected ship"}
                </h2>
                <p>{description}</p>
              </div>
              <button
                className={styles.iconButton}
                onClick={clearSelection}
                aria-label="Close ship inspector"
              >
                <X size={18} aria-hidden="true" />
              </button>
            </header>
            {inspector}
          </aside>
        )}
      </div>
      <Dialog.Root
        open={(compact ? compactOpen : !wide) && selectionInScope}
        onOpenChange={(open) => {
          if (!open) {
            setCompactOpen(false);
            selectShip("");
          }
        }}
      >
        <Dialog.Portal>
          <Dialog.Overlay className={styles.dialogOverlay} />
          <Dialog.Content
            className={styles.dialogContent}
            id="ship-inspector"
            onOpenAutoFocus={() => {
              if (
                document.activeElement instanceof HTMLElement &&
                document.activeElement !== document.body &&
                !document.activeElement.closest("#ship-inspector")
              ) {
                returnFocus.current = document.activeElement;
              }
            }}
            onCloseAutoFocus={(event) => {
              event.preventDefault();
              restoreFocus();
            }}
          >
            <header className={styles.inspectorHeading}>
              <div>
                <Dialog.Title className="sr-only">
                  {selected?.detail.ship.name ?? "Selected ship"}
                </Dialog.Title>
                <Dialog.Description className={styles.dialogDescription}>
                  {description}
                </Dialog.Description>
              </div>
              <Dialog.Close asChild>
                <button
                  className={styles.iconButton}
                  aria-label="Close ship inspector"
                >
                  <X size={18} aria-hidden="true" />
                </button>
              </Dialog.Close>
            </header>
            {inspector}
          </Dialog.Content>
        </Dialog.Portal>
      </Dialog.Root>
    </div>
  );
}
