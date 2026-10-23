"use client";

import {
  ArrowLeftRight,
  ChartCandlestick,
  Layers,
  MapPin,
  Package,
  ReceiptText,
  Shield,
  TrendingUp,
} from "lucide-react";
import { useState } from "react";
import { dataset, number, record, rows, text } from "@/lib/data";
import { count, dateTime, humanize, money, percent } from "@/lib/format";
import type { Account, JsonRecord } from "@/lib/types";
import {
  Badge,
  DataTable,
  Dataset,
  DatasetStatus,
  SearchField,
  Section,
  StatusBadge,
} from "./ui";
import { SettlementLedger } from "./settlements";
import styles from "./operations.module.css";

const DESKS = [
  { id: "spot", label: "Spot orders", icon: ChartCandlestick },
  { id: "fills", label: "Fills", icon: ArrowLeftRight },
  { id: "insurance", label: "Insurance", icon: Shield },
  { id: "futures", label: "Futures", icon: TrendingUp },
  { id: "depth", label: "Market depth", icon: Layers },
] as const;

export function ExchangeView({ account }: { account: Account }) {
  const [tab, setTab] = useState<(typeof DESKS)[number]["id"]>("spot");
  const [search, setSearch] = useState("");
  const [side, setSide] = useState("");
  const matches = (row: JsonRecord) =>
    `${row.commodity_id ?? ""} ${row.port_id ?? ""} ${row.origin_port_id ?? ""} ${row.dest_port_id ?? ""} ${row.ship_id ?? ""}`
      .toLowerCase()
      .includes(search.toLowerCase());
  const orders = rows(dataset(account, "orders")).filter(
    (row) => matches(row) && (!side || row.side === side),
  );
  const fills = rows(dataset(account, "fills")).filter(
    (row) => matches(row) && (!side || row.side === side),
  );
  const insurance = rows(dataset(account, "insurance")).filter(matches);
  const futures = rows(dataset(account, "futures")).filter(
    (row) => matches(row) && (!side || row.side === side),
  );

  return (
    <div className={styles.workspace}>
      <div className={styles.tabs} role="group" aria-label="Exchange desks">
        {DESKS.map(({ id, label, icon: Icon }) => (
          <button
            key={id}
            type="button"
            aria-pressed={tab === id}
            onClick={() => setTab(id)}
          >
            <Icon size={15} strokeWidth={1.7} aria-hidden="true" />
            {label}
          </button>
        ))}
      </div>
      <div className={styles.toolbar}>
        <SearchField
          value={search}
          onChange={setSearch}
          placeholder="Port, commodity or ship"
          label="Filter exchange records"
        />
        {tab !== "insurance" && tab !== "depth" && (
          <label>
            Side
            <select
              value={side}
              onChange={(event) => setSide(event.target.value)}
            >
              <option value="">All sides</option>
              <option value="bid">Bid</option>
              <option value="ask">Ask</option>
            </select>
          </label>
        )}
        {(search || side) && (
          <button
            type="button"
            className={styles.reset}
            onClick={() => {
              setSearch("");
              setSide("");
            }}
          >
            Clear filters
          </button>
        )}
      </div>
      {tab === "spot" && (
        <Section title="Resting orders" icon={ChartCandlestick}>
          <Dataset account={account} name="orders">
            <DataTable
              label="Recorded spot orders"
              searchable={false}
              data={orders}
              rowKey={(row, index) => text(row.id, String(index))}
              empty="No recorded orders match these filters"
              emptyIcon={ChartCandlestick}
              columns={[
                {
                  label: "Market",
                  value: (row) =>
                    `${text(row.commodity_id, "")} ${text(row.port_id, "")}`,
                  render: (row) => (
                    <span className={styles.primary}>
                      <span className={styles.iconLabel}>
                        <Package
                          size={13}
                          strokeWidth={1.7}
                          aria-hidden="true"
                        />
                        {humanize(text(row.commodity_id))}
                      </span>
                      <small>{humanize(text(row.port_id))}</small>
                    </span>
                  ),
                },
                {
                  label: "Side",
                  value: (row) => text(row.side, "") || undefined,
                  render: (row) => (
                    <Badge
                      icon={ArrowLeftRight}
                      tone={row.side === "bid" ? "good" : "neutral"}
                    >
                      {humanize(text(row.side))}
                    </Badge>
                  ),
                },
                {
                  label: "Limit per ton",
                  value: (row) => number(row.price_pence),
                  render: (row) => money(number(row.price_pence)),
                  className: styles.numeric,
                },
                {
                  label: "Quantity",
                  value: (row) => number(row.quantity),
                  render: (row) => `${count(number(row.quantity))} t`,
                  className: styles.numeric,
                },
                {
                  label: "Details",
                  render: (row) => (
                    <details className={styles.inlineDetails}>
                      <summary>Order details</summary>
                      <dl className={styles.details}>
                        <div>
                          <dt>Filled</dt>
                          <dd>{count(number(row.filled_quantity))} t</dd>
                        </div>
                        <div>
                          <dt>Updated</dt>
                          <dd>{dateTime(text(row.updated_at))}</dd>
                        </div>
                        <div>
                          <dt>Order ID</dt>
                          <dd>{text(row.id, "Not recorded")}</dd>
                        </div>
                      </dl>
                    </details>
                  ),
                },
              ]}
            />
          </Dataset>
        </Section>
      )}
      {tab === "fills" && (
        <>
          <Section title="Executed fills" icon={ArrowLeftRight}>
            <p className={styles.note}>
              Executions show price, quantity and customs, not realized profit.
            </p>
            <Dataset account={account} name="fills">
              <DataTable
                label="Recorded trade fills"
                searchable={false}
                data={fills}
                rowKey={(row, index) => text(row.id, String(index))}
                empty="No recorded fills match these filters"
                emptyIcon={ArrowLeftRight}
                columns={[
                  {
                    label: "Market",
                    value: (row) =>
                      `${text(row.commodity_id, "")} ${text(row.port_id, "")}`,
                    render: (row) => (
                      <span className={styles.primary}>
                        <strong>{humanize(text(row.commodity_id))}</strong>
                        <small>{humanize(text(row.port_id))}</small>
                      </span>
                    ),
                  },
                  {
                    label: "Side",
                    value: (row) => text(row.side, "") || undefined,
                    render: (row) => (
                      <Badge icon={ArrowLeftRight}>
                        {humanize(text(row.side))}
                      </Badge>
                    ),
                  },
                  {
                    label: "Price per ton",
                    value: (row) => number(row.price_pence),
                    render: (row) => money(number(row.price_pence)),
                    className: styles.numeric,
                  },
                  {
                    label: "Quantity",
                    value: (row) => number(row.quantity),
                    render: (row) => `${count(number(row.quantity))} t`,
                    className: styles.numeric,
                  },
                  {
                    label: "Details",
                    render: (row) => (
                      <details className={styles.inlineDetails}>
                        <summary>Execution details</summary>
                        <dl className={styles.details}>
                          <div>
                            <dt>Executed</dt>
                            <dd>{dateTime(text(row.created_at))}</dd>
                          </div>
                          <div>
                            <dt>Customs</dt>
                            <dd>{money(number(row.customs_duty_pence))}</dd>
                          </div>
                          <div>
                            <dt>Role</dt>
                            <dd>
                              {row.is_maker === true
                                ? "Maker"
                                : row.is_maker === false
                                  ? "Taker"
                                  : "Not recorded"}
                            </dd>
                          </div>
                          <div>
                            <dt>Fill ID</dt>
                            <dd>{text(row.id, "Not recorded")}</dd>
                          </div>
                        </dl>
                      </details>
                    ),
                  },
                ]}
              />
            </Dataset>
          </Section>
          <details className={styles.disclosure}>
            <summary>
              <ReceiptText size={15} strokeWidth={1.7} aria-hidden="true" />
              Realized settlements
            </summary>
            <SettlementLedger account={account} search={search} />
          </details>
        </>
      )}
      {tab === "insurance" && (
        <Section title="Voyage insurance" icon={Shield}>
          <Dataset account={account} name="insurance">
            <DataTable
              label="Recorded voyage policies"
              searchable={false}
              data={insurance}
              rowKey={(row, index) => text(row.id, String(index))}
              empty="No recorded policies match these filters"
              emptyIcon={Shield}
              columns={[
                {
                  label: "Voyage",
                  value: (row) =>
                    `${text(row.origin_port_id, "")} ${text(row.dest_port_id, "")}`,
                  render: (row) => (
                    <span className={styles.primary}>
                      <span>{humanize(text(row.origin_port_id))}</span>
                      <small>To {humanize(text(row.dest_port_id))}</small>
                    </span>
                  ),
                },
                {
                  label: "Status",
                  value: (row) => text(row.status, "") || undefined,
                  render: (row) => <StatusBadge status={text(row.status)} />,
                },
                {
                  label: "Insured value",
                  value: (row) => number(row.insured_value_pence),
                  render: (row) => money(number(row.insured_value_pence)),
                  className: styles.numeric,
                },
                {
                  label: "Premium",
                  value: (row) => number(row.total_premium_pence),
                  render: (row) => money(number(row.total_premium_pence)),
                  className: styles.numeric,
                },
                {
                  label: "Terms",
                  render: (row) => (
                    <details className={styles.inlineDetails}>
                      <summary>Policy terms</summary>
                      <dl className={styles.details}>
                        <div>
                          <dt>Ship ID</dt>
                          <dd>{text(row.ship_id, "Not recorded")}</dd>
                        </div>
                        <div>
                          <dt>Hull value when insured</dt>
                          <dd>{money(number(row.hull_value_pence))}</dd>
                        </div>
                        <div>
                          <dt>Premium rate</dt>
                          <dd>{percent(number(row.premium_bps))}</dd>
                        </div>
                        <div>
                          <dt>Covered</dt>
                          <dd>{percent(number(row.covered_bps))}</dd>
                        </div>
                        <div>
                          <dt>Expires</dt>
                          <dd>{dateTime(text(row.expires_at))}</dd>
                        </div>
                      </dl>
                    </details>
                  ),
                },
              ]}
            />
          </Dataset>
          <p className={styles.note}>
            Coverage applies to the named destination. Insured value is not a
            current cargo valuation or guaranteed payout.
          </p>
        </Section>
      )}
      {tab === "futures" && (
        <Section title="Futures commitments" icon={TrendingUp}>
          <Dataset account={account} name="futures">
            <DataTable
              label="Recorded physical futures"
              searchable={false}
              data={futures}
              rowKey={(row, index) => text(row.id, String(index))}
              empty="No recorded futures match these filters"
              emptyIcon={TrendingUp}
              columns={[
                {
                  label: "Market",
                  value: (row) =>
                    `${text(row.commodity_id, "")} ${text(row.port_id, "")}`,
                  render: (row) => (
                    <span className={styles.primary}>
                      <strong>{humanize(text(row.commodity_id))}</strong>
                      <small>{humanize(text(row.port_id))}</small>
                    </span>
                  ),
                },
                {
                  label: "Position",
                  value: (row) =>
                    `${text(row.side, "")} ${text(row.status, "")}`,
                  render: (row) => (
                    <span className={styles.primary}>
                      <Badge icon={TrendingUp}>
                        {humanize(text(row.side))}
                      </Badge>
                      <StatusBadge status={text(row.status)} />
                    </span>
                  ),
                },
                {
                  label: "Lot",
                  value: (row) => number(row.lot_tons),
                  render: (row) => `${count(number(row.lot_tons))} t`,
                  className: styles.numeric,
                },
                {
                  label: "Strike per ton",
                  value: (row) => number(row.strike_price_pence),
                  render: (row) => money(number(row.strike_price_pence)),
                  className: styles.numeric,
                },
                {
                  label: "Terms",
                  render: (row) => (
                    <details className={styles.inlineDetails}>
                      <summary>Commitment terms</summary>
                      <dl className={styles.details}>
                        <div>
                          <dt>Buyer margin</dt>
                          <dd>{money(number(row.buyer_margin_pence))}</dd>
                        </div>
                        <div>
                          <dt>Seller margin</dt>
                          <dd>{money(number(row.seller_margin_pence))}</dd>
                        </div>
                        <div>
                          <dt>Matures</dt>
                          <dd>{dateTime(text(row.matures_at))}</dd>
                        </div>
                      </dl>
                    </details>
                  ),
                },
              ]}
            />
          </Dataset>
        </Section>
      )}
      {tab === "depth" && <MarketDepth account={account} search={search} />}
    </div>
  );
}

function MarketDepth({
  account,
  search,
}: {
  account: Account;
  search: string;
}) {
  const books = dataset<Record<string, unknown>>(account, "books") ?? {};
  const sources = Object.entries(books).map(([portID, value]) => ({
    port_id: portID,
    book: record(value),
  }));
  const markets = sources
    .flatMap(({ port_id, book }) =>
      rows(book.books).map((commodity): JsonRecord & { port_id: string } => ({
        ...commodity,
        port_id,
        visibility: book.visibility,
        customs_bps: book.customs_bps,
        as_of: book.as_of,
      })),
    )
    .filter((row) =>
      `${row.port_id} ${text(row.commodity_id, "")}`
        .toLowerCase()
        .includes(search.toLowerCase()),
    );

  return (
    <Section title="Recorded market depth" icon={Layers}>
      <p className={styles.note}>
        Visibility limits recorded depth. Storage prices can be a day old;
        catalog base prices are never used as bids or asks.
      </p>
      <Dataset account={account} name="books">
        <DataTable
          label="Recorded commodity order books"
          searchable={false}
          data={markets}
          rowKey={(row) => `${row.port_id}:${text(row.commodity_id)}`}
          empty="No recorded markets match this filter"
          emptyIcon={Layers}
          columns={[
            {
              label: "Market",
              value: (row) => `${row.port_id} ${text(row.commodity_id, "")}`,
              render: (row) => (
                <span className={styles.primary}>
                  <strong>{humanize(text(row.commodity_id))}</strong>
                  <small className={styles.iconLabel}>
                    <MapPin size={13} strokeWidth={1.7} aria-hidden="true" />
                    {humanize(row.port_id)}
                  </small>
                </span>
              ),
            },
            {
              label: "Best bid per ton",
              value: (row) => number(record(row.best_bid).price_pence),
              render: (row) => (
                <span className={styles.primary}>
                  {money(number(record(row.best_bid).price_pence))}
                  <small>
                    {count(number(record(row.best_bid).quantity))} t
                  </small>
                </span>
              ),
              className: styles.numeric,
            },
            {
              label: "Best ask per ton",
              value: (row) => number(record(row.best_ask).price_pence),
              render: (row) => (
                <span className={styles.primary}>
                  {money(number(record(row.best_ask).price_pence))}
                  <small>
                    {count(number(record(row.best_ask).quantity))} t
                  </small>
                </span>
              ),
              className: styles.numeric,
            },
            {
              label: "Depth",
              render: (row) => {
                const depth = record(row.depth);
                const levels: JsonRecord[] = [
                  ...rows(depth.bids).map((level) => ({
                    ...level,
                    side: "bid",
                  })),
                  ...rows(depth.asks).map((level) => ({
                    ...level,
                    side: "ask",
                  })),
                ];
                return (
                  <details className={styles.inlineDetails}>
                    <summary>Market details</summary>
                    <dl className={styles.details}>
                      <div>
                        <dt>Visibility</dt>
                        <dd>{humanize(text(row.visibility))}</dd>
                      </div>
                      <div>
                        <dt>Source price time</dt>
                        <dd>{dateTime(text(row.as_of))}</dd>
                      </div>
                      <div>
                        <dt>Customs</dt>
                        <dd>{percent(number(row.customs_bps))}</dd>
                      </div>
                      <div>
                        <dt>NPC stock</dt>
                        <dd>{count(number(depth.npc_current_stock))} t</dd>
                      </div>
                    </dl>
                    <DatasetStatus
                      account={account}
                      name={`books/${row.port_id}`}
                    />
                    <DataTable
                      label={`${humanize(row.port_id)} ${humanize(text(row.commodity_id))} depth levels`}
                      data={levels}
                      rowKey={(level, index) =>
                        text(level.order_id, `${level.side}:${index}`)
                      }
                      empty="No further depth levels recorded"
                      columns={[
                        {
                          label: "Side",
                          value: (level) => text(level.side),
                          render: (level) => humanize(text(level.side)),
                        },
                        {
                          label: "Price per ton",
                          value: (level) => number(level.price_pence),
                          render: (level) => money(number(level.price_pence)),
                          className: styles.numeric,
                        },
                        {
                          label: "Remaining",
                          value: (level) => number(level.remaining_quantity),
                          render: (level) =>
                            `${count(number(level.remaining_quantity))} t`,
                          className: styles.numeric,
                        },
                      ]}
                    />
                  </details>
                );
              },
            },
          ]}
        />
        <details className={styles.disclosure}>
          <summary>
            <Layers size={15} strokeWidth={1.7} aria-hidden="true" />
            Market sources, including empty books
          </summary>
          <DataTable
            label="Recorded market sources"
            data={sources}
            rowKey={(row) => row.port_id}
            columns={[
              {
                label: "Port",
                value: (row) => row.port_id,
                render: (row) => humanize(row.port_id),
              },
              {
                label: "Visibility",
                value: (row) => text(row.book.visibility, "") || undefined,
                render: (row) => humanize(text(row.book.visibility)),
              },
              {
                label: "Source price time",
                value: (row) => text(row.book.as_of, "") || undefined,
                render: (row) => dateTime(text(row.book.as_of)),
              },
              {
                label: "Customs",
                value: (row) => number(row.book.customs_bps),
                render: (row) => percent(number(row.book.customs_bps)),
                className: styles.numeric,
              },
              {
                label: "Freshness",
                render: (row) => (
                  <DatasetStatus
                    account={account}
                    name={`books/${row.port_id}`}
                  />
                ),
              },
            ]}
          />
        </details>
      </Dataset>
    </Section>
  );
}
