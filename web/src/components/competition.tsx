"use client";

import { Users } from "lucide-react";
import { useMemo } from "react";
import { useNow } from "@/app/providers";
import { useCompetition } from "@/lib/data";
import { age, count, dateTime, humanize, money } from "@/lib/format";
import type { CompetitionActivity, CompetitionPort } from "@/lib/types";
import { Badge, EmptyState, Section } from "./ui";
import styles from "./competition.module.css";

export function CompetitionView({ accountID }: { accountID: string }) {
  const query = useCompetition(accountID);
  const now = useNow();
  const summary = query.data;

  return (
    <Section title="Competition" icon={Users} id="competition">
      <p className={styles.caption}>
        Partial, name-based traffic over the last 24 real hours, not live ship
        positions; unobserved does not mean zero.
      </p>
      {query.isError && (
        <p className={styles.warning} role="status">
          Competition could not be loaded. {query.error.message}
          {summary && " The last recorded summary is retained below."}
        </p>
      )}
      {!summary ? (
        !query.isError && (
          <EmptyState
            title="Loading recorded traffic"
            description="Reading this account’s saved observations; no game requests are made."
          />
        )
      ) : (
        <>
          {!summary.own_companies_complete && (
            <p className={styles.warning} role="status">
              Our configured company names are not all recorded. Foreign counts
              remain unknown until they can be excluded safely.
            </p>
          )}
          {summary.events_truncated && (
            <p className={styles.warning} role="status">
              The event limit was reached. These observations are a lower bound.
            </p>
          )}
          <div className={styles.window}>
            <span>
              {dateTime(summary.from)} to {dateTime(summary.to)}
            </span>
            <span>
              Summary {age(summary.generated_at, now)}
              {now - Date.parse(summary.generated_at) >= 120_000 && (
                <>; updates delayed</>
              )}
            </span>
          </div>
          {summary.regions.length === 0 ? (
            <EmptyState
              title="No recorded port catalog"
              description="Competition will appear when this account has a saved world catalog."
            />
          ) : (
            <div className={styles.regions}>
              <div className={styles.headings} aria-hidden="true">
                <span>Region</span>
                <span>Observed foreign traffic</span>
                <span>Leading company</span>
                <span>Coverage</span>
              </div>
              {summary.regions.map((region) => (
                <details
                  key={`${accountID}:${region.id}`}
                  className={styles.region}
                >
                  <summary className={styles.row}>
                    <strong>{humanize(region.id)}</strong>
                    <CompetitionCounts activity={region} />
                    <span className={styles.leading}>
                      {region.top_competitors[0]?.name ?? "—"}
                    </span>
                    <span className={styles.coverage}>
                      {region.coverage === "not_observed"
                        ? summary.own_companies_complete
                          ? "No quay coverage"
                          : "Owner names unknown"
                        : `${region.observed_ports} of ${region.ports.length} ports; partial`}
                    </span>
                  </summary>
                  <div className={styles.ports}>
                    <p className={styles.caption}>
                      {region.last_seen
                        ? `Last foreign sighting ${dateTime(region.last_seen)}.`
                        : region.coverage === "partial"
                          ? "No foreign names recorded."
                          : "Traffic not observed."}
                    </p>
                    {region.ports.map((port) => (
                      <CompetitionPortRow key={port.id} port={port} now={now} />
                    ))}
                  </div>
                </details>
              ))}
            </div>
          )}
        </>
      )}
    </Section>
  );
}

function CompetitionCounts({ activity }: { activity: CompetitionActivity }) {
  if (activity.foreign_companies === null || activity.foreign_ships === null) {
    return <span className={styles.unknown}>Not observed</span>;
  }
  return (
    <span>
      {count(activity.foreign_companies)} companies,{" "}
      {count(activity.foreign_ships)} ships
    </span>
  );
}

function CompetitionPortRow({
  port,
  now,
}: {
  port: CompetitionPort;
  now: number;
}) {
  const companies = useMemo(() => {
    const grouped = new Map<
      string,
      { name: string; ships: number; lastSeen: string }
    >();
    for (const ship of port.ships) {
      const company = grouped.get(ship.company_name);
      if (!company) {
        grouped.set(ship.company_name, {
          name: ship.company_name,
          ships: 1,
          lastSeen: ship.last_seen,
        });
        continue;
      }
      company.ships++;
      if (Date.parse(ship.last_seen) > Date.parse(company.lastSeen)) {
        company.lastSeen = ship.last_seen;
      }
    }
    return [...grouped.values()].sort(
      (a, b) => b.ships - a.ships || a.name.localeCompare(b.name),
    );
  }, [port.ships]);
  return (
    <details className={styles.port}>
      <summary className={styles.row}>
        <strong>{port.name || humanize(port.id)}</strong>
        <CompetitionCounts activity={port} />
        <span className={styles.leading}>
          {port.top_competitors[0]?.name ?? "—"}
        </span>
        <span className={styles.coverage}>
          {port.last_seen
            ? `Seen ${dateTime(port.last_seen)}`
            : port.coverage === "partial"
              ? "Quay sample only"
              : port.quay_observed_at
                ? "Foreign counts unknown"
                : "No quay coverage"}
        </span>
      </summary>
      <div className={styles.portDetail}>
        <p className={styles.caption}>
          Public sample {dateTime(port.observed_at)} (includes us; window
          unspecified).{" "}
          {port.source_stale && (
            <Badge tone="warning">
              {port.observed_at
                ? "Port source stale"
                : "Port source unavailable"}
            </Badge>
          )}
        </p>
        <dl className={styles.publicActivity}>
          <div>
            <dt>Public tons</dt>
            <dd>{count(port.player_volume_tons)}</dd>
          </div>
          <div>
            <dt>Turnover</dt>
            <dd>{money(port.player_turnover_pence)}</dd>
          </div>
          <div>
            <dt>Berths</dt>
            <dd>
              {count(port.occupied_berths)} / {count(port.effective_berths)}
            </dd>
          </div>
          <div>
            <dt>Queued</dt>
            <dd>{count(port.queued_ships)}</dd>
          </div>
        </dl>
        {companies.length > 0 && (
          <>
            <table
              className={styles.companies}
              aria-label={`Companies observed at ${port.name}`}
            >
              <thead>
                <tr>
                  <th scope="col">Company</th>
                  <th scope="col">Ships seen</th>
                  <th scope="col">Last seen</th>
                </tr>
              </thead>
              <tbody>
                {companies.slice(0, 5).map((company) => (
                  <tr key={company.name}>
                    <th scope="row">{company.name}</th>
                    <td>{count(company.ships)}</td>
                    <td>
                      <time
                        dateTime={company.lastSeen}
                        title={dateTime(company.lastSeen)}
                      >
                        {age(company.lastSeen, now)}
                      </time>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            {companies.length > 5 && (
              <p className={styles.caption}>
                and {count(companies.length - 5)} more
              </p>
            )}
          </>
        )}
      </div>
    </details>
  );
}
