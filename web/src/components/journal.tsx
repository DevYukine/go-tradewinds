"use client";

import { BookOpen } from "lucide-react";
import { useMemo } from "react";
import { dataset, fleet, useHistory } from "@/lib/data";
import { count, dateTime, humanize, money } from "@/lib/format";
import { journalCashAmount, type JournalEntry } from "@/lib/journal";
import type { Account, Catalog, CompanyProfile } from "@/lib/types";
import { Badge, DataTable, Section, SpriteImage, type Column } from "./ui";
import styles from "./operations.module.css";

export function Journal({ account }: { account: Account }) {
  const history = useHistory<JournalEntry>(account.id, "journal");
  const company =
    dataset<CompanyProfile>(account, "profile")?.company.name ?? account.label;
  const catalog = dataset<Catalog>(account, "catalog");
  const ships = useMemo(
    () => new Map(fleet(account).map(({ ship }) => [ship.id, ship.name])),
    [account],
  );
  const entries = useMemo(
    () =>
      (history.data ?? [])
        .filter((entry) => entry.account_id === account.id)
        .sort((left, right) => Date.parse(right.at) - Date.parse(left.at)),
    [history.data, account.id],
  );
  const columns = useMemo<Column<JournalEntry>[]>(() => {
    const vesselName = (entry: JournalEntry) =>
      entry.ship_id ? (ships.get(entry.ship_id) ?? entry.ship_id) : "Company";
    const portName = (id: string) =>
      catalog?.ports.find((port) => port.id === id)?.name ?? humanize(id);
    const commodities = new Map(
      catalog?.commodities.map((commodity) => [commodity.id, commodity.name]),
    );

    return [
      {
        label: "Recording time",
        className: styles.journalTimeColumn,
        value: (entry) => entry.at,
        render: (entry) => (
          <time dateTime={entry.at}>{dateTime(entry.at)}</time>
        ),
      },
      {
        label: "Vessel",
        className: styles.journalVesselColumn,
        value: (entry) => `${vesselName(entry)} ${entry.ship_id ?? ""}`,
        render: (entry) => (
          <div className="vessel-identity">
            {entry.ship_id && <SpriteImage name={vesselName(entry)} />}
            <span>{vesselName(entry)}</span>
          </div>
        ),
      },
      {
        label: "Activity",
        className: styles.journalActivityColumn,
        value: (entry) =>
          [
            entry.kind === "settlement"
              ? "Voyage settled"
              : humanize(
                  entry.kind.replace(/^action:/, "").replaceAll(".", "_"),
                ),
            entry.summary,
            entry.origin && portName(entry.origin),
            entry.destination && portName(entry.destination),
            entry.commodity &&
              (commodities.get(entry.commodity) ?? humanize(entry.commodity)),
            entry.quantity_tons,
          ].join(" "),
        render: (entry) => (
          <div className={styles.journalActivity}>
            <strong>
              {entry.summary ||
                (entry.kind === "settlement"
                  ? "Voyage settled"
                  : humanize(
                      entry.kind.replace(/^action:/, "").replaceAll(".", "_"),
                    ))}
            </strong>
            {(entry.origin ||
              entry.destination ||
              entry.commodity ||
              entry.quantity_tons !== undefined) && (
              <div className={styles.journalContext}>
                {(entry.origin || entry.destination) && (
                  <span>
                    {entry.origin && entry.destination
                      ? `${portName(entry.origin)} to ${portName(entry.destination)}`
                      : portName(entry.origin || entry.destination || "")}
                  </span>
                )}
                {(entry.commodity || entry.quantity_tons !== undefined) && (
                  <span>
                    {entry.quantity_tons !== undefined &&
                      `${count(entry.quantity_tons)} t `}
                    {entry.commodity
                      ? (commodities.get(entry.commodity) ??
                        humanize(entry.commodity))
                      : "cargo"}
                  </span>
                )}
              </div>
            )}
          </div>
        ),
      },
      {
        label: "Cash / realized net",
        className: styles.journalCashColumn,
        value: journalCashAmount,
        render: (entry) => {
          const amount = journalCashAmount(entry);
          const unknownCosts =
            (entry.kind === "settlement" && entry.costs_known !== true) ||
            (entry.amount_kind === "realized_net" &&
              entry.costs_known === false);
          const tone =
            amount === undefined || amount === 0
              ? undefined
              : entry.amount_kind === "charge" ||
                  entry.amount_kind === "cash_out" ||
                  amount < 0
                ? "negative"
                : "positive";
          if (unknownCosts) {
            return (
              <div className={styles.journalCash}>
                <span>Voyage net unknown</span>
                <small>Costs not fully recorded; not zero.</small>
              </div>
            );
          }
          const label =
            entry.kind === "settlement" || entry.amount_kind === "realized_net"
              ? "Realized voyage net"
              : entry.amount_kind === "charge"
                ? "Charge"
                : entry.amount_kind === "cash_in"
                  ? "Cash in"
                  : entry.amount_kind === "cash_out"
                    ? "Cash out"
                    : "Recorded amount";
          if (amount === undefined) {
            return entry.amount_kind || entry.kind === "settlement" ? (
              <span className={styles.journalMissing}>
                {label}: amount not recorded
              </span>
            ) : (
              <span
                className={styles.journalMissing}
                aria-label="No cash amount recorded; not evidence of zero"
                title="No cash amount recorded; not evidence of zero"
              >
                —
              </span>
            );
          }
          return (
            <div className={styles.journalCash}>
              <span>
                <strong className={tone}>{money(amount)}</strong>{" "}
                <span className={styles.journalCashLabel}>{label}</span>
              </span>
            </div>
          );
        },
      },
    ];
  }, [ships, catalog]);

  return (
    <Section
      title={`${company} journal`}
      icon={BookOpen}
      className={`${styles.section} ${styles.journal}`}
      actions={<Badge>{count(entries.length)} recorded entries</Badge>}
    >
      <p className={styles.note}>
        Latest 500 entries, newest first; times in UTC. Cash in, cash out and
        charges are movements, not profit. Realized voyage net is shown only
        with recorded costs; rows can overlap and must not be added together.
      </p>
      <div className={styles.coverage} role="status">
        {history.isPending
          ? "Loading activity."
          : history.data
            ? `Retrieved ${dateTime(new Date(history.dataUpdatedAt).toISOString())}.`
            : "No journal sample is available."}
        {history.isFetching && !history.isPending && " Updating."}
        {entries.length >= 500 && " 500-entry limit reached."}
      </div>
      {history.error && (
        <div className={styles.warning} role="alert">
          Journal unavailable: {history.error.message}{" "}
          {history.data
            ? "Previously retrieved entries remain visible."
            : "No recorded activity can be shown."}
        </div>
      )}
      <DataTable
        key={account.id}
        label={`${company} journal`}
        columns={columns}
        data={entries}
        rowKey={(entry) => entry.id}
        empty={
          history.isPending
            ? "Retrieving recorded company activity"
            : history.error
              ? "Journal history unavailable"
              : "No activity recorded in the returned journal sample"
        }
        emptyIcon={BookOpen}
      />
      <details className={styles.coverage}>
        <summary>Journal coverage</summary>
        <p>
          This bounded sample is not a complete company ledger; older entries
          may be omitted. Events are retained for seven days. Completed actions
          and voyage settlements are durable.
        </p>
        <p>
          Recording time uses the local save time for events, the last recorded
          update for actions, and the recorded completion time for settlements.
          Charges, cash in and cash out are individual movements, not profit.
          Realized voyage net includes recorded voyage costs.
        </p>
      </details>
    </Section>
  );
}
