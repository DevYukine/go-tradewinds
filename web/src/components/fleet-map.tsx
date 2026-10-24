"use client";

import { Anchor, Compass, List } from "lucide-react";
import dynamic from "next/dynamic";
import { useMemo } from "react";
import { dataset, fleet } from "@/lib/data";
import { count, humanize, money, percent } from "@/lib/format";
import { PORT_COORDINATES } from "@/lib/geography";
import type { Account, Catalog, PortDetail } from "@/lib/types";
import { useUI } from "@/lib/ui-store";
import { DatasetStatus } from "./ui";
import styles from "./fleet.module.css";

const FleetChart = dynamic(() => import("./map/fleet-chart"), {
  ssr: false,
  loading: () => (
    <div className={styles.loadingChart} role="status">
      Loading the fleet chart.
    </div>
  ),
});

export function FleetMap({
  accounts,
  catalog,
  snapshotUTC,
}: {
  accounts: Account[];
  catalog?: Catalog;
  snapshotUTC: string;
}) {
  const selectedPortID = useUI((state) => state.selectedPortID);
  const setView = useUI((state) => state.setView);
  const selectedCatalog =
    catalog ??
    accounts
      .map((account) => dataset<Catalog>(account, "catalog"))
      .find((value) => value !== undefined);
  const ships = useMemo(
    () => accounts.flatMap((account) => fleet(account)),
    [accounts],
  );
  const port = selectedCatalog?.ports.find(
    (item) => item.id === selectedPortID,
  );
  const portObservations = useMemo(
    () =>
      accounts.flatMap((account) => {
        const detail = dataset<PortDetail>(account, `ports/${selectedPortID}`);
        return detail ? [{ account, detail }] : [];
      }),
    [accounts, selectedPortID],
  );
  const unmapped =
    selectedCatalog?.ports.filter((item) => !PORT_COORDINATES[item.id]) ?? [];

  return (
    <section className={styles.chartSection} aria-label="Working fleet chart">
      <header className={styles.sectionHeading}>
        <div>
          <h2>
            <Compass size={17} strokeWidth={1.7} aria-hidden="true" /> Fleet map
          </h2>
          <p>
            {count(ships.length)} recorded ships;{" "}
            {count(
              ships.filter((detail) => detail.ship.status === "sailing").length,
            )}{" "}
            at sea
          </p>
        </div>
        <button className={styles.button} onClick={() => setView("fleet")}>
          <List size={15} strokeWidth={1.7} aria-hidden="true" /> Vessel ledger
        </button>
      </header>
      <FleetChart
        accounts={accounts}
        catalog={selectedCatalog}
        snapshotUTC={snapshotUTC}
      />
      <div className={styles.chartCaption}>
        <p>
          Sea positions are estimates; paused or stale estimates freeze.
          Scheduled arrival is not confirmation. Dashed lines show the recorded
          route, not a measured track.
        </p>
        <a
          href="https://www.naturalearthdata.com/about/terms-of-use/"
          target="_blank"
          rel="noreferrer"
        >
          Approximate Natural Earth coastline; not for navigation
        </a>
      </div>
      {unmapped.length > 0 && (
        <p className={styles.note}>
          No chart coordinates for{" "}
          {unmapped.map((item) => item.name).join(", ")}. Use port focus or the
          vessel ledger for their recorded facts.
        </p>
      )}
      {selectedPortID && (
        <section className={styles.portDetail} aria-label="Selected port">
          <h3>
            <Anchor size={15} strokeWidth={1.7} aria-hidden="true" />{" "}
            {port?.name ?? humanize(selectedPortID)}
          </h3>
          {port && (
            <dl className={styles.portDetails}>
              <div>
                <dt>Region</dt>
                <dd>{humanize(port.banking_region)}</dd>
              </div>
              <div>
                <dt>Customs</dt>
                <dd>{percent(port.customs_bps)}</dd>
              </div>
              <div>
                <dt>Shipyard</dt>
                <dd>{port.has_shipyard ? "In catalog" : "Not in catalog"}</dd>
              </div>
            </dl>
          )}
          {portObservations.length === 0 ? (
            <p className={styles.note}>
              Harbour conditions have not been recorded in this scope. Catalog
              definitions do not establish current berth occupancy.
            </p>
          ) : (
            portObservations.map(({ account, detail }) => (
              <div className={styles.sourceAccount} key={account.id}>
                <strong>{account.label}</strong>
                <DatasetStatus
                  account={account}
                  name={`ports/${selectedPortID}`}
                />
                <dl className={styles.portDetails}>
                  <div>
                    <dt>Occupied berths</dt>
                    <dd>
                      {count(detail.occupied_berths)} /{" "}
                      {count(detail.effective_berths)}
                    </dd>
                  </div>
                  <div>
                    <dt>Queued ships</dt>
                    <dd>{count(detail.queued_ships)}</dd>
                  </div>
                  <div>
                    <dt>Berth dues / game day</dt>
                    <dd>{money(detail.berth_dues_per_day_pence)}</dd>
                  </div>
                  <div>
                    <dt>Health</dt>
                    <dd>{percent(detail.health_score_bps)}</dd>
                  </div>
                  <div>
                    <dt>Visibility</dt>
                    <dd>{humanize(detail.visibility)}</dd>
                  </div>
                </dl>
              </div>
            ))
          )}
        </section>
      )}
    </section>
  );
}
