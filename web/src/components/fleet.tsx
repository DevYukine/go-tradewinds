"use client";

import * as Dialog from "@radix-ui/react-dialog";
import {
  Anchor,
  Box,
  ScrollText,
  Navigation,
  Ship,
  TriangleAlert,
  Utensils,
  X,
} from "lucide-react";
import { useId, useMemo, useRef, useState, useSyncExternalStore } from "react";
import { useNow } from "@/app/providers";
import { dataset, fleet, shipSource } from "@/lib/data";
import { count, dateTime, humanize } from "@/lib/format";
import { observationLabel, shipOperation } from "@/lib/operations";
import type {
  Account,
  Catalog,
  CompanyProfile,
  DashboardData,
  Decision,
  ShipDetail,
} from "@/lib/types";
import { useUI } from "@/lib/ui-store";
import { FleetMap } from "./fleet-map";
import { ShipCurrentHold } from "./operations-overview";
import {
  Badge,
  DataTable,
  DatasetStatus,
  EmptyState,
  Progress,
  StatusBadge,
  SpriteImage,
  type Column,
} from "./ui";
import styles from "./fleet.module.css";

interface FleetRow {
  account: Account;
  detail: ShipDetail;
  company: string;
  className: string;
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
}: {
  accounts: Account[];
  catalog?: Catalog;
  dashboard: DashboardData;
  compact?: boolean;
}) {
  const now = useNow();
  const [statusFilter, setStatusFilter] = useState("");
  const [portFilter, setPortFilter] = useState("");
  const selectedShipID = useUI((state) => state.selectedShipID);
  const selectedShipAccountID = useUI((state) => state.selectedShipAccountID);
  const selectShip = useUI((state) => state.selectShip);
  const wide = useSyncExternalStore(subscribeWidth, wideSnapshot, serverWidth);
  const ledgerHeading = useRef<HTMLHeadingElement | null>(null);
  const returnFocus = useRef<HTMLElement | null>(null);
  const headingID = useId();
  const rows = useMemo(
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
  const selected = rows.find(
    (row) =>
      row.account.id === selectedShipAccountID &&
      row.detail.ship.id === selectedShipID,
  );
  const selectionInScope = Boolean(
    selectedShipID &&
    accounts.some((account) => account.id === selectedShipAccountID),
  );
  const selectedCatalog = selected
    ? (dataset<Catalog>(selected.account, "catalog") ?? catalog)
    : catalog;
  const statuses = useMemo(
    () =>
      [
        ...new Set([
          ...rows.map((row) => row.detail.ship.status),
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
            ...rows.flatMap((row) => [
              row.detail.ship.current_port_id,
              destination(row.detail),
            ]),
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
            ? detail.voyage?.legs?.some((leg) => leg.starving)
            : detail.ship.status === statusFilter);
        const matchesPort =
          !portFilter ||
          detail.ship.current_port_id === portFilter ||
          destination(detail) === portFilter;
        return matchesStatus && matchesPort;
      }),
    [rows, statusFilter, portFilter],
  );
  const latestDecision = useMemo(
    () =>
      dashboard.decisions.reduce<Decision | undefined>((latest, decision) => {
        if (
          decision.account_id !== selectedShipAccountID ||
          decision.ship_id !== selectedShipID
        ) {
          return latest;
        }
        return !latest ||
          Date.parse(decision.created_at) > Date.parse(latest.created_at)
          ? decision
          : latest;
      }, undefined),
    [dashboard.decisions, selectedShipAccountID, selectedShipID],
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
            onClick={() => selectShip(row.detail.ship.id, row.account.id)}
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
        label: "Observed activity",
        value: ({ detail }) =>
          `${humanize(detail.ship.status)}${detail.voyage?.legs?.some((leg) => leg.starving) ? " starving" : ""}`,
        render: ({ account, detail }) => {
          const operation = shipOperation(account, detail, now);
          return (
            <span>
              {operation.observation.current ? (
                <StatusBadge status={detail.ship.status} />
              ) : (
                <Badge>Last recorded: {operation.activity}</Badge>
              )}
              <span className={styles.cellSubtext}>
                {observationLabel(operation.observation.state)}
              </span>
              {operation.starving && (
                <span className={styles.cellSubtext}>
                  Starvation in recorded voyage estimate
                </span>
              )}
            </span>
          );
        },
      },
      {
        label: "Ship hold",
        value: ({ account, detail }) =>
          shipOperation(account, detail, now)
            .holds.map((hold) => hold.reason)
            .join("; ") || undefined,
        render: ({ account, detail }) => (
          <ShipCurrentHold operation={shipOperation(account, detail, now)} />
        ),
      },
      {
        label: "Port / destination",
        value: ({ detail }) => {
          const portID = destination(detail) ?? detail.ship.current_port_id;
          return portID ? humanize(portID) : undefined;
        },
        className: styles.location,
        render: ({ detail }) =>
          detail.ship.status === "sailing"
            ? destination(detail)
              ? `To ${humanize(destination(detail))}`
              : "Destination not recorded"
            : detail.ship.current_port_id
              ? humanize(detail.ship.current_port_id)
              : "Not recorded",
      },
      {
        label: "Cargo",
        value: ({ detail }) => detail.cargo_used_tons,
        render: ({ detail }) => (
          <span>
            {count(detail.cargo_used_tons)} t
            <span className={styles.cellSubtext}>
              {count(detail.effective_hold_capacity_tons)} t capacity
            </span>
          </span>
        ),
        className: styles.numeric,
      },
      {
        label: "Stores",
        value: ({ detail }) => detail.stores?.sea_days_remaining,
        render: ({ detail }) =>
          detail.stores
            ? `${count(detail.stores.sea_days_remaining)} sea days`
            : "Not recorded",
        className: styles.numeric,
      },
      {
        label: "Scheduled arrival",
        value: ({ detail }) => {
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
    [selectedShipID, selectedShipAccountID, selectShip, now],
  );
  const description = selected
    ? `${selected.company}, configured account ${selected.account.label}`
    : "The selected ship is not present in the latest recorded fleet for this scope.";
  const inspector = selected ? (
    <ShipInspector
      account={selected.account}
      detail={selected.detail}
      catalog={selectedCatalog}
      decision={latestDecision}
    />
  ) : (
    <EmptyState
      icon={Ship}
      title="Selected ship is unavailable"
      description="The selection is retained. Choose another vessel or wait for a complete fleet recording."
    />
  );

  return (
    <div className={`${styles.workspace} ${compact ? styles.compact : ""}`}>
      <div className={styles.chartLayout}>
        {compact && (
          <FleetMap
            accounts={accounts}
            catalog={catalog}
            snapshotUTC={dashboard.updated_at}
          />
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
              {selectionInScope && (
                <button
                  className={styles.button}
                  onClick={() => selectShip("")}
                >
                  Clear ship selection
                </button>
              )}
            </header>
            <div className={styles.observations}>
              {accounts.map((account) => (
                <div className={styles.observation} key={account.id}>
                  <strong>{account.label}</strong>
                  <DatasetStatus account={account} name="ships" />
                </div>
              ))}
            </div>
            <div className={styles.filters}>
              <label className={styles.filterField}>
                Status and warnings
                <select
                  value={statusFilter}
                  onChange={(event) => setStatusFilter(event.target.value)}
                >
                  <option value="">All recorded states</option>
                  {statuses.map((status) => (
                    <option key={status} value={status}>
                      {humanize(status)}
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
        {(compact || wide) && selectionInScope && (
          <aside
            className={styles.inspector}
            aria-label="Selected ship inspector"
          >
            <header className={styles.inspectorHeading}>
              <div>
                <h2>Vessel details</h2>
                <p>{description}</p>
              </div>
              <button
                className={styles.iconButton}
                onClick={() => selectShip("")}
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
        open={!compact && !wide && selectionInScope}
        onOpenChange={(open) => {
          if (!open) selectShip("");
        }}
      >
        <Dialog.Portal>
          <Dialog.Overlay className={styles.dialogOverlay} />
          <Dialog.Content
            className={styles.dialogContent}
            onOpenAutoFocus={() => {
              returnFocus.current =
                document.activeElement instanceof HTMLElement
                  ? document.activeElement
                  : null;
            }}
            onCloseAutoFocus={(event) => {
              event.preventDefault();
              const target = returnFocus.current?.isConnected
                ? returnFocus.current
                : ledgerHeading.current;
              target?.focus({ preventScroll: true });
            }}
          >
            <header className={styles.inspectorHeading}>
              <div>
                <Dialog.Title className={styles.dialogTitle}>
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

export function ShipInspector({
  account,
  detail,
  catalog,
  decision,
}: {
  account: Account;
  detail: ShipDetail;
  catalog?: Catalog;
  decision?: Decision;
}) {
  const now = useNow();
  const shipClass = catalog?.ship_classes.find(
    (item) => item.id === detail.ship.ship_class,
  );
  const starvingLegs = detail.voyage?.legs?.filter((leg) => leg.starving);
  const cargo = detail.cargo_manifest
    ? Object.entries(detail.cargo_manifest)
    : undefined;
  const capacityKnown =
    Number.isFinite(detail.cargo_used_tons) &&
    Number.isFinite(detail.effective_hold_capacity_tons) &&
    detail.effective_hold_capacity_tons > 0;
  const hasVoyage =
    detail.ship.status === "sailing" ||
    Boolean(
      detail.voyage || detail.ship.leg_departs_at || detail.ship.leg_arrives_at,
    ) ||
    (detail.route_legs?.length ?? 0) > 0;
  const hasBerth =
    Boolean(detail.ship.berthed_at) ||
    Boolean(detail.ship.dock_queued_at) ||
    Boolean(detail.ship.turnaround_unlocks_at);

  return (
    <div className={styles.inspectorBody}>
      <div className={styles.shipIdentity}>
        <SpriteImage name={detail.ship.name} />
        <div>
          <strong>{detail.ship.name}</strong>
          <span className={styles.identity}>
            <StatusBadge status={detail.ship.status} />
            {shipClass?.name ?? humanize(detail.ship.ship_class)}
          </span>
        </div>
      </div>
      <DatasetStatus
        account={account}
        name={shipSource(account, detail.ship.id)}
      />
      <ShipCurrentHold operation={shipOperation(account, detail, now)} />
      {starvingLegs && starvingLegs.length > 0 && (
        <p className={styles.warning}>
          <TriangleAlert size={15} strokeWidth={1.7} aria-hidden="true" />
          <span>
            Starvation recorded in voyage estimate on{" "}
            {starvingLegs.length === 1 ? "leg" : "legs"}{" "}
            {starvingLegs.map((leg) => leg.leg_idx + 1).join(", ")}. This is a
            recorded warning, not a local prediction.
          </span>
        </p>
      )}
      <div className={styles.inspectorColumns}>
        <div className={styles.inspectorColumn}>
          <section className={styles.inspectorSection}>
            <h3>
              <Box size={17} strokeWidth={1.7} aria-hidden="true" /> Cargo
              manifest
            </h3>
            {cargo ? (
              cargo.length ? (
                <ul className={styles.cargo}>
                  {cargo.map(([commodity, quantity]) => (
                    <li key={commodity}>
                      <span>
                        {catalog?.commodities.find(
                          (item) => item.id === commodity,
                        )?.name ?? humanize(commodity)}
                      </span>
                      <strong>{count(quantity)} t</strong>
                    </li>
                  ))}
                </ul>
              ) : (
                <p className={styles.note}>
                  No trade cargo in the recorded manifest.
                </p>
              )
            ) : (
              <p className={styles.note}>Cargo manifest not recorded.</p>
            )}
            <p className={styles.note}>
              {count(detail.cargo_used_tons)} /{" "}
              {count(detail.effective_hold_capacity_tons)} t of effective cargo
              capacity.
            </p>
            {capacityKnown && (
              <Progress
                value={
                  (detail.cargo_used_tons /
                    detail.effective_hold_capacity_tons) *
                  100
                }
                label={`${detail.ship.name} cargo capacity used`}
              />
            )}
          </section>
          <section className={styles.inspectorSection}>
            <h3>
              <Utensils size={17} strokeWidth={1.7} aria-hidden="true" /> Crew
              and stores
            </h3>
            <dl className={styles.facts}>
              <div>
                <dt>Provisions aboard</dt>
                <dd>{count(detail.stores?.provisions_tons)} t</dd>
              </div>
              <div>
                <dt>Sea endurance</dt>
                <dd>{count(detail.stores?.sea_days_remaining)} game days</dd>
              </div>
              <div>
                <dt>Exact provision units</dt>
                <dd>{count(detail.ship.stores_provisions_units)}</dd>
              </div>
              <div>
                <dt>Crew required</dt>
                <dd>{count(shipClass?.crew_required)}</dd>
              </div>
            </dl>
            <p className={styles.note}>
              Stores occupy hold space and feed the crew. They are separate from
              trade cargo. One provision unit is 1/10,000 crew-day.
            </p>
          </section>
          <details
            className={`${styles.inspectorSection} ${styles.inspectorDisclosures}`}
          >
            <summary>
              <Ship size={17} strokeWidth={1.7} aria-hidden="true" /> Hull and
              cannons
            </summary>
            <dl className={styles.facts}>
              <div>
                <dt>Hull rating</dt>
                <dd>{humanize(shipClass?.hull_category)}</dd>
              </div>
              <div>
                <dt>Hull hold capacity</dt>
                <dd>{count(detail.hold_capacity_tons)} t</dd>
              </div>
              <div>
                <dt>Mounted cannons</dt>
                <dd>
                  {count(detail.ship.cannons_count)} /{" "}
                  {count(shipClass?.max_cannons)}
                </dd>
              </div>
              <div>
                <dt>Base catalog speed</dt>
                <dd>
                  {shipClass
                    ? count(shipClass.base_speed_centiknots / 100)
                    : "Unknown"}{" "}
                  kn
                </dd>
              </div>
              <div>
                <dt>Created</dt>
                <dd>{dateTime(detail.ship.created_at)}</dd>
              </div>
              <div>
                <dt>Damage detail</dt>
                <dd>Not included in the recorded ship dataset</dd>
              </div>
              <div>
                <dt>Construction detail</dt>
                <dd>
                  No build schedule is included in the recorded ship dataset
                </dd>
              </div>
            </dl>
            <p className={styles.note}>
              Lifecycle status is shown as recorded. No damage, construction
              completion, or hull health is inferred from cargo capacity.
            </p>
          </details>
        </div>
        <div className={styles.inspectorColumn}>
          <section className={styles.inspectorSection}>
            <h3>
              <Navigation size={17} strokeWidth={1.7} aria-hidden="true" />{" "}
              Recorded voyage
            </h3>
            {hasVoyage ? (
              <>
                <dl className={styles.facts}>
                  <div>
                    <dt>Current port</dt>
                    <dd>{humanize(detail.ship.current_port_id)}</dd>
                  </div>
                  <div>
                    <dt>Destination</dt>
                    <dd>
                      {detail.ship.status === "sailing"
                        ? humanize(destination(detail))
                        : "No active voyage recorded"}
                    </dd>
                  </div>
                  <div>
                    <dt>Recorded current leg</dt>
                    <dd>
                      {detail.ship.status === "sailing" &&
                      Number.isFinite(detail.ship.current_leg_idx)
                        ? detail.ship.current_leg_idx + 1
                        : "No active leg recorded"}
                    </dd>
                  </div>
                  <div>
                    <dt>Leg departure</dt>
                    <dd>{dateTime(detail.ship.leg_departs_at)}</dd>
                  </div>
                  <div>
                    <dt>Scheduled leg arrival</dt>
                    <dd>{dateTime(detail.ship.leg_arrives_at)}</dd>
                  </div>
                  <div>
                    <dt>Scheduled voyage arrival</dt>
                    <dd>{dateTime(detail.voyage?.estimated_arrival_at)}</dd>
                  </div>
                  <div>
                    <dt>Recorded voyage speed</dt>
                    <dd>
                      {detail.voyage
                        ? `${count(detail.voyage.speed_centiknots / 100)} kn`
                        : "Not recorded"}
                    </dd>
                  </div>
                  <div>
                    <dt>Starving legs</dt>
                    <dd>
                      {starvingLegs
                        ? starvingLegs.length
                          ? starvingLegs
                              .map((leg) => leg.leg_idx + 1)
                              .join(", ")
                          : "None in recorded estimate"
                        : "Voyage estimate not recorded"}
                    </dd>
                  </div>
                  <div>
                    <dt>Convoy</dt>
                    <dd>
                      {detail.ship.convoy_id ??
                        (detail.ship.convoy_id === null
                          ? "None recorded"
                          : "Not recorded")}
                    </dd>
                  </div>
                </dl>
                <p className={styles.note}>
                  Schedule timestamps are real UTC, not accelerated game time.
                  Arrival and leg changes require another recording.
                </p>
                {(detail.route_legs?.length ?? 0) > 0 && (
                  <ol className={styles.legs} aria-label="Recorded route legs">
                    {detail.route_legs.map((leg, index) => {
                      const estimate = detail.voyage?.legs?.find(
                        (item) => item.leg_idx === index,
                      );
                      return (
                        <li
                          key={`${index}:${leg.from_node}:${leg.to_node}`}
                          aria-current={
                            detail.ship.status === "sailing" &&
                            index === detail.ship.current_leg_idx
                              ? "step"
                              : undefined
                          }
                        >
                          {humanize(leg.from_node)} to {humanize(leg.to_node)}
                          <small>
                            {count(leg.distance_nm)} nm,{" "}
                            {humanize(leg.edge_class)}
                            {leg.is_sound_toll ? ", Sound toll" : ""}
                          </small>
                          {estimate && (
                            <small>
                              {count(estimate.speed_centiknots / 100)} kn; ETA{" "}
                              {dateTime(estimate.estimated_arrival_at)}
                              {estimate.starving ? "; starving" : ""}
                            </small>
                          )}
                        </li>
                      );
                    })}
                  </ol>
                )}
              </>
            ) : (
              <p className={styles.note}>
                No active voyage recorded
                {detail.ship.current_port_id
                  ? `; recorded port: ${humanize(detail.ship.current_port_id)}.`
                  : "."}
              </p>
            )}
          </section>
          <details
            className={`${styles.inspectorSection} ${styles.inspectorDisclosures}`}
          >
            <summary>
              <Anchor size={17} strokeWidth={1.7} aria-hidden="true" /> Berth
              and turnaround
            </summary>
            {hasBerth ? (
              <dl className={styles.facts}>
                <div>
                  <dt>Berthed at</dt>
                  <dd>
                    {detail.ship.berthed_at === null
                      ? "No berth timestamp recorded"
                      : dateTime(detail.ship.berthed_at)}
                  </dd>
                </div>
                <div>
                  <dt>Berth queue entered</dt>
                  <dd>
                    {detail.ship.dock_queued_at === null
                      ? "No queue recorded"
                      : dateTime(detail.ship.dock_queued_at)}
                  </dd>
                </div>
                <div>
                  <dt>Turnaround unlock</dt>
                  <dd>
                    {detail.ship.turnaround_unlocks_at === null
                      ? "No lock recorded"
                      : dateTime(detail.ship.turnaround_unlocks_at)}
                  </dd>
                </div>
                <div>
                  <dt>Ship record updated</dt>
                  <dd>{dateTime(detail.ship.updated_at)}</dd>
                </div>
              </dl>
            ) : (
              <p className={styles.note}>
                No berth, queue or turnaround timestamps recorded. Ship record
                updated {dateTime(detail.ship.updated_at)}.
              </p>
            )}
            <p className={styles.note}>
              The ship record timestamp is a game fact, not the recording
              service&apos;s recording time.
            </p>
          </details>
          <section className={styles.inspectorSection}>
            <h3>
              <ScrollText size={17} strokeWidth={1.7} aria-hidden="true" />{" "}
              Latest recorded decision
            </h3>
            {decision ? (
              <>
                <StatusBadge status={decision.kind} />
                <p className={styles.note}>{dateTime(decision.created_at)}</p>
                <p className={styles.decisionReason}>{decision.reason}</p>
                <p className={styles.note}>
                  Historical evaluation in the dashboard&apos;s recent records,
                  not the current ship hold or a guaranteed next action.
                </p>
              </>
            ) : (
              <p className={styles.note}>
                No decision for this vessel appears in the dashboard&apos;s
                recent records. No next action is inferred.
              </p>
            )}
          </section>
        </div>
      </div>
    </div>
  );
}
