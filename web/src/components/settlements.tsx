"use client";

import {
  AlertTriangle,
  FileText,
  Package,
  ReceiptText,
  Ship,
  ShoppingBag,
  Wrench,
} from "lucide-react";
import { useMemo, useState } from "react";
import { useNow } from "@/app/providers";
import { fleet, useHistory } from "@/lib/data";
import { count, dateTime, humanize, money } from "@/lib/format";
import type { Account, Settlement } from "@/lib/types";
import { Badge, DataTable, Section, SpriteImage } from "./ui";
import styles from "./operations.module.css";
import viewStyles from "./world-views.module.css";

function realizedNet(item: Settlement): number | undefined {
  return item.costs_known &&
    item.proceeds_pence !== null &&
    item.proceeds_unknown !== true
    ? item.proceeds_pence -
        item.acquisition_pence -
        item.fees_pence -
        item.provisions_pence -
        item.expenses_pence
    : undefined;
}

export function SettlementLedger({
  account,
  search,
}: {
  account: Account;
  search: string;
}) {
  const history = useHistory<Settlement>(account.id, "settlements");
  const [shipID, setShipID] = useState("");
  const [kind, setKind] = useState("");
  const [range, setRange] = useState("all");
  const now = useNow();
  const ships = fleet(account);
  const sample = useMemo(
    () =>
      (history.data ?? [])
        .filter((item) => item.account_id === account.id)
        .sort(
          (left, right) =>
            Date.parse(left.completed_at) - Date.parse(right.completed_at),
        ),
    [history.data, account.id],
  );
  const cutoff =
    range === "24h" ? now - 86_400_000 : range === "7d" ? now - 604_800_000 : 0;
  const entries = sample.filter(
    (item) =>
      (!shipID || item.ship_id === shipID) &&
      (!kind || item.kind === kind) &&
      Date.parse(item.completed_at) >= cutoff &&
      `${item.ship_id} ${item.lane} ${item.origin} ${item.destination} ${item.commodity}`
        .toLowerCase()
        .includes(search.toLowerCase()),
  );
  const known = entries.filter((item) => realizedNet(item) !== undefined);
  const net = known.length
    ? known.reduce((sum, item) => sum + (realizedNet(item) ?? 0), 0)
    : undefined;
  const shipOptions = [
    ...new Set([
      ...ships.map((item) => item.ship.id),
      ...sample.map((item) => item.ship_id),
    ]),
  ].sort();
  const kinds = [...new Set(sample.map((item) => item.kind))].sort();
  const byShip = new Map<
    string,
    {
      ship_id: string;
      count: number;
      known: number;
      quantity: number;
      proceeds: number | null;
      net: number;
    }
  >();
  for (const item of entries) {
    const summary = byShip.get(item.ship_id) ?? {
      ship_id: item.ship_id,
      count: 0,
      known: 0,
      quantity: 0,
      proceeds: 0,
      net: 0,
    };
    summary.count++;
    summary.quantity += item.quantity_tons;
    summary.proceeds =
      summary.proceeds === null ||
      item.proceeds_pence === null ||
      item.proceeds_unknown === true
        ? null
        : summary.proceeds + item.proceeds_pence;
    const itemNet = realizedNet(item);
    if (itemNet !== undefined) {
      summary.known++;
      summary.net += itemNet;
    }
    byShip.set(item.ship_id, summary);
  }
  const shipName = (id: string) =>
    ships.find((item) => item.ship.id === id)?.ship.name ?? id;

  return (
    <Section
      title="Realized voyage settlements"
      icon={ReceiptText}
      className={styles.section}
      actions={
        <Badge icon={ReceiptText}>{count(entries.length)} records</Badge>
      }
    >
      <p className={styles.note}>
        Completed missions only. Net deducts acquisition, fees, provisions and
        expenses from proceeds. Unknown costs and proceeds stay unknown.
      </p>
      <div className={styles.toolbar}>
        <label>
          Ship
          <select
            value={shipID}
            onChange={(event) => setShipID(event.target.value)}
          >
            <option value="">All recorded ships</option>
            {shipOptions.map((id) => (
              <option key={id} value={id}>
                {shipName(id)}
              </option>
            ))}
          </select>
        </label>
        <label>
          Mission kind
          <select
            value={kind}
            onChange={(event) => setKind(event.target.value)}
          >
            <option value="">All kinds</option>
            {kinds.map((value) => (
              <option key={value} value={value}>
                {humanize(value)}
              </option>
            ))}
          </select>
        </label>
        <label>
          Completed
          <select
            value={range}
            onChange={(event) => setRange(event.target.value)}
          >
            <option value="all">All returned records</option>
            <option value="24h">Last 24 hours in sample</option>
            <option value="7d">Last 7 days in sample</option>
          </select>
        </label>
        {(shipID || kind || range !== "all") && (
          <button
            type="button"
            className={styles.reset}
            onClick={() => {
              setShipID("");
              setKind("");
              setRange("all");
            }}
          >
            Clear settlement filters
          </button>
        )}
      </div>
      <div className={styles.coverage} role="status">
        {history.isPending
          ? "Retrieving recorded settlements"
          : `${count(sample.length)} returned records, capped at 500 per request.`}
        {sample.length > 0 && (
          <>
            {" "}
            Completed {dateTime(sample[0].completed_at)} to{" "}
            {dateTime(sample.at(-1)?.completed_at)}.
          </>
        )}
        {history.isFetching && !history.isPending && " Updating."} This is a
        bounded sample, not complete period coverage.
      </div>
      {history.error && (
        <div className={styles.warning} role="alert">
          Settlement history unavailable: {history.error.message}{" "}
          {history.data
            ? "Retained records remain visible."
            : "No settlement sample is available."}
        </div>
      )}
      {sample.length >= 500 && (
        <p className={styles.warning}>
          The history response limit was reached. Older settlements may be
          omitted.
        </p>
      )}
      <dl className={styles.facts}>
        <div>
          <dt>Known-cost sample net</dt>
          <dd
            className={
              net === undefined || net === 0
                ? undefined
                : net < 0
                  ? "negative"
                  : "positive"
            }
          >
            {money(net)}
          </dd>
        </div>
        <div>
          <dt>Known-cost settlements</dt>
          <dd>{count(known.length)}</dd>
        </div>
        <div>
          <dt>Costs or proceeds unknown</dt>
          <dd>{count(entries.length - known.length)}</dd>
        </div>
        <div>
          <dt>Sample proceeds</dt>
          <dd>
            {entries.some(
              (item) =>
                item.proceeds_pence === null || item.proceeds_unknown === true,
            )
              ? "Proceeds unknown"
              : money(
                  entries.length
                    ? entries.reduce(
                        (sum, item) => sum + item.proceeds_pence!,
                        0,
                      )
                    : undefined,
                )}
          </dd>
        </div>
      </dl>
      <p className={styles.note}>
        Subtotals follow these filters, not table search or pagination, and do
        not establish complete period profit.
      </p>
      <details className={viewStyles.disclosure}>
        <summary>
          <Ship size={17} strokeWidth={1.7} aria-hidden="true" /> Per-ship
          summary
        </summary>
        <DataTable
          label="Per-ship aggregates within the filtered settlement sample"
          searchable={false}
          data={[...byShip.values()]}
          rowKey={(row) => row.ship_id}
          empty="No settlements in this filtered sample"
          columns={[
            {
              label: "Ship",
              value: (row) => shipName(row.ship_id),
              render: (row) => (
                <span className={viewStyles.inline}>
                  <SpriteImage name={shipName(row.ship_id)} />
                  {shipName(row.ship_id)}
                </span>
              ),
            },
            {
              label: "Settlements",
              value: (row) => row.count,
              render: (row) => count(row.count),
              className: styles.numeric,
            },
            {
              label: "Quantity",
              value: (row) => row.quantity,
              render: (row) => `${count(row.quantity)} t`,
              className: styles.numeric,
            },
            {
              label: "Proceeds subtotal",
              value: (row) => row.proceeds ?? undefined,
              render: (row) =>
                row.proceeds === null
                  ? "Proceeds unknown"
                  : money(row.proceeds),
              className: styles.numeric,
            },
            {
              label: "Known-cost net subtotal",
              value: (row) => (row.known ? row.net : undefined),
              render: (row) => (
                <span
                  className={
                    !row.known || row.net === 0
                      ? undefined
                      : row.net < 0
                        ? "negative"
                        : "positive"
                  }
                >
                  {money(row.known ? row.net : undefined)}
                </span>
              ),
              className: styles.numeric,
            },
            {
              label: "Known-cost count",
              value: (row) => row.known,
              render: (row) => count(row.known),
              className: styles.numeric,
            },
            {
              label: "Unknown costs or proceeds",
              value: (row) => row.count - row.known,
              render: (row) => count(row.count - row.known),
              className: styles.numeric,
            },
          ]}
        />
      </details>
      <section className={styles.subsection} aria-label="Settlement records">
        <h3>Settlement ledger</h3>
        <DataTable
          label="Recorded realized voyage settlement sample"
          searchable={false}
          data={entries}
          rowKey={(item) => item.id}
          empty={
            history.isPending
              ? "Retrieving settlements"
              : "No matching settlements in the returned sample"
          }
          columns={[
            {
              label: "Completed",
              value: (item) => item.completed_at || undefined,
              render: (item) => dateTime(item.completed_at),
            },
            {
              label: "Ship",
              value: (item) => shipName(item.ship_id),
              render: (item) => (
                <span className={viewStyles.inline}>
                  <SpriteImage name={shipName(item.ship_id)} />
                  {shipName(item.ship_id)}
                </span>
              ),
            },
            {
              label: "Lane",
              value: (item) =>
                item.lane || `${item.origin} ${item.destination}`,
              render: (item) => (
                <span className={styles.primary}>
                  {humanize(item.origin)} to {humanize(item.destination)}
                  <small>{humanize(item.lane)}</small>
                </span>
              ),
            },
            {
              label: "Kind",
              value: (item) => item.kind || undefined,
              render: (item) => (
                <Badge
                  icon={
                    item.kind === "charter"
                      ? FileText
                      : item.kind === "freight"
                        ? Package
                        : item.kind === "spot"
                          ? ShoppingBag
                          : item.kind === "recovery"
                            ? Wrench
                            : ReceiptText
                  }
                >
                  {humanize(item.kind)}
                </Badge>
              ),
            },
            {
              label: "Commodity",
              value: (item) => item.commodity || undefined,
              render: (item) => humanize(item.commodity),
            },
            {
              label: "Quantity",
              value: (item) => item.quantity_tons,
              render: (item) => `${count(item.quantity_tons)} t`,
              className: styles.numeric,
            },
            {
              label: "Proceeds",
              value: (item) => item.proceeds_pence ?? undefined,
              render: (item) =>
                item.proceeds_pence === null || item.proceeds_unknown
                  ? "Proceeds unknown"
                  : money(item.proceeds_pence),
              className: styles.numeric,
            },
            {
              label: "Realized net",
              value: realizedNet,
              render: (item) => {
                const amount = realizedNet(item);
                return amount === undefined ? (
                  <Badge tone="warning" icon={AlertTriangle}>
                    {item.proceeds_pence === null || item.proceeds_unknown
                      ? "Proceeds unknown"
                      : "Costs unknown"}
                  </Badge>
                ) : (
                  <span
                    className={
                      amount < 0
                        ? "negative"
                        : amount > 0
                          ? "positive"
                          : undefined
                    }
                  >
                    {money(amount)}
                  </span>
                );
              },
              className: styles.numeric,
            },
            {
              label: "Game duration",
              value: (item) => item.game_seconds,
              render: (item) => `${count(item.game_seconds / 86_400)} days`,
              className: styles.numeric,
            },
            {
              label: "Started",
              value: (item) => item.started_at || undefined,
              render: (item) => dateTime(item.started_at),
            },
            {
              label: "Breakdown",
              render: (item) => (
                <details>
                  <summary>Costs and record IDs</summary>
                  <dl className={styles.details}>
                    <div>
                      <dt>Acquisition</dt>
                      <dd>
                        {item.costs_known
                          ? money(item.acquisition_pence)
                          : "Costs unknown"}
                      </dd>
                    </div>
                    <div>
                      <dt>Fees</dt>
                      <dd>{money(item.fees_pence)}</dd>
                    </div>
                    <div>
                      <dt>Provisions</dt>
                      <dd>{money(item.provisions_pence)}</dd>
                    </div>
                    <div>
                      <dt>Expenses</dt>
                      <dd>{money(item.expenses_pence)}</dd>
                    </div>
                    <div>
                      <dt>Settlement ID</dt>
                      <dd className={styles.identifier}>{item.id}</dd>
                    </div>
                    <div>
                      <dt>Mission ID</dt>
                      <dd className={styles.identifier}>{item.mission_id}</dd>
                    </div>
                    <div>
                      <dt>Action ID</dt>
                      <dd className={styles.identifier}>
                        {item.action_id || "Not recorded"}
                      </dd>
                    </div>
                    {item.event_id && (
                      <div>
                        <dt>Event ID</dt>
                        <dd className={styles.identifier}>{item.event_id}</dd>
                      </div>
                    )}
                  </dl>
                </details>
              ),
            },
          ]}
        />
      </section>
    </Section>
  );
}

export function RecentSettlements({ account }: { account: Account }) {
  const history = useHistory<Settlement>(account.id, "settlements");
  const ships = fleet(account);
  const recent = useMemo(
    () =>
      (history.data ?? [])
        .filter((item) => item.account_id === account.id)
        .sort(
          (left, right) =>
            Date.parse(right.completed_at) - Date.parse(left.completed_at),
        )
        .slice(0, 10),
    [history.data, account.id],
  );

  return (
    <section className={styles.subsection} aria-label="Recent settlements">
      <h3 className={viewStyles.inline}>
        <ReceiptText size={17} strokeWidth={1.7} aria-hidden="true" />
        Recent settlements
      </h3>
      <p className={styles.note}>
        Up to ten latest settlements from the returned sample, capped at 500
        records. This is not complete period coverage. Net deducts acquisition,
        fees, provisions and expenses; unknown costs stay unknown.
      </p>
      {history.error && (
        <p className={styles.warning} role="alert">
          Settlement history unavailable: {history.error.message}{" "}
          {history.data
            ? "Retained records remain visible."
            : "No settlement sample is available."}
        </p>
      )}
      <DataTable
        label="Recent realized voyage settlements"
        data={recent}
        rowKey={(item) => item.id}
        empty={
          history.isPending
            ? "Retrieving settlements"
            : "No settlements in the returned sample"
        }
        columns={[
          {
            label: "Completed",
            value: (item) => item.completed_at || undefined,
            render: (item) => dateTime(item.completed_at),
          },
          {
            label: "Vessel",
            value: (item) =>
              ships.find((detail) => detail.ship.id === item.ship_id)?.ship
                .name ?? item.ship_id,
            render: (item) => {
              const name =
                ships.find((detail) => detail.ship.id === item.ship_id)?.ship
                  .name ?? item.ship_id;
              return (
                <span className={viewStyles.inline}>
                  <SpriteImage name={name} />
                  {name}
                </span>
              );
            },
          },
          {
            label: "Route",
            value: (item) => `${item.origin} ${item.destination}`,
            render: (item) =>
              `${humanize(item.origin)} to ${humanize(item.destination)}`,
          },
          {
            label: "Proceeds",
            value: (item) => item.proceeds_pence ?? undefined,
            render: (item) =>
              item.proceeds_pence === null || item.proceeds_unknown
                ? "Proceeds unknown"
                : money(item.proceeds_pence),
            className: styles.numeric,
          },
          {
            label: "Realized net",
            value: realizedNet,
            render: (item) => {
              const amount = realizedNet(item);
              return amount === undefined ? (
                <Badge tone="warning" icon={AlertTriangle}>
                  {item.proceeds_pence === null || item.proceeds_unknown
                    ? "Proceeds unknown"
                    : "Costs unknown"}
                </Badge>
              ) : (
                <span
                  className={
                    amount < 0
                      ? "negative"
                      : amount > 0
                        ? "positive"
                        : undefined
                  }
                >
                  {money(amount)}
                </span>
              );
            },
            className: styles.numeric,
          },
        ]}
      />
    </section>
  );
}
