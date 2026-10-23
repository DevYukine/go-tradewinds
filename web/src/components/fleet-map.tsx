"use client";

import { useQuery } from "@tanstack/react-query";
import { Anchor, Compass, List } from "lucide-react";
import dynamic from "next/dynamic";
import { useMemo } from "react";
import { dataset, fleet } from "@/lib/data";
import { count, humanize, money, percent } from "@/lib/format";
import { PORT_COORDINATES } from "@/lib/geography";
import type {
  Account,
  Catalog,
  Clock,
  DashboardData,
  PortDetail,
} from "@/lib/types";
import { useUI } from "@/lib/ui-store";
import { DatasetStatus } from "./ui";
import styles from "./fleet.module.css";

const FleetChart = dynamic(() => import("./map/fleet-chart"), {
  ssr: false,
  loading: () => (
    <div className={styles.loadingChart} role="status">
      Loading the fleet chart. Use the vessel selector to inspect recorded
      ships.
    </div>
  ),
});

export function FleetMap({
  accounts,
  catalog,
}: {
  accounts: Account[];
  catalog?: Catalog;
}) {
  const { data } = useQuery<DashboardData>({
    queryKey: ["dashboard"],
    enabled: false,
  });
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
            <Compass size={17} strokeWidth={1.7} aria-hidden="true" /> Working
            chart
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
        snapshotUTC={data?.updated_at}
      />
      <div className={styles.chartCaption}>
        <p>
          Sea positions are schedule estimates, not measured locations.
          Estimates freeze when paused or stale. Scheduled arrival never
          confirms arrival. Dashed routes connect recorded nodes, not a recorded
          track.
        </p>
        <a
          href="https://www.naturalearthdata.com/about/terms-of-use/"
          target="_blank"
          rel="noreferrer"
        >
          Natural Earth public-domain land
        </a>
      </div>
      {unmapped.length > 0 && (
        <p className={styles.note}>
          No chart coordinates for{" "}
          {unmapped.map((item) => item.name).join(", ")}. Use the port selector
          and vessel selector instead.
        </p>
      )}
      <details className={styles.chartSources}>
        <summary>Chart sources and recording status</summary>
        <p className={styles.note}>
          Local 1:50m coastline, approximate harbour and sea-zone coordinates,
          no external tiles. Not for navigation.
        </p>
        {accounts.map((account) => {
          const clock = dataset<Clock>(account, "clock");
          return (
            <div className={styles.sourceAccount} key={account.id}>
              <strong>{account.label}</strong>
              <span>
                Clock{" "}
                {clock
                  ? clock.paused
                    ? "paused; estimates frozen"
                    : "recorded; estimates need current recordings"
                  : "not recorded; voyage position unavailable"}
              </span>
              <DatasetStatus account={account} name="clock" />
              <DatasetStatus account={account} name="catalog" />
            </div>
          );
        })}
      </details>
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
