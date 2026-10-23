"use client";

import {
  AlertTriangle,
  Anchor,
  CheckCircle2,
  ChevronLeft,
  ChevronRight,
  Hammer,
  MapPin,
  Package,
  Ship,
} from "lucide-react";
import { useState } from "react";
import { dataset, fleet, shipSource } from "@/lib/data";
import { count, dateTime, humanize, money, percent } from "@/lib/format";
import type { Account, Catalog, Clock, YardQuote } from "@/lib/types";
import {
  Badge,
  DataTable,
  Dataset,
  DatasetStatus,
  EmptyState,
  Section,
  StatusBadge,
} from "./ui";
import styles from "./operations.module.css";

export function ShipyardView({ account }: { account: Account }) {
  const [portFilter, setPortFilter] = useState("");
  const [classFilter, setClassFilter] = useState("");
  const [availability, setAvailability] = useState("");
  const [page, setPage] = useState(0);
  const catalog = dataset<Catalog>(account, "catalog");
  const clock = dataset<Clock>(account, "clock");
  const quotes = Object.entries(
    dataset<Record<string, YardQuote>>(account, "yard_quotes") ?? {},
  ).map(([key, quote]) => ({ ...quote, observationKey: key }));
  const filtered = quotes.filter(
    (quote) =>
      (!portFilter || quote.port_id === portFilter) &&
      (!classFilter || quote.ship_class === classFilter) &&
      (!availability || String(quote.available) === availability),
  );
  const yards = catalog?.ports.filter((port) => port.has_shipyard) ?? [];
  const portOptions = [
    ...new Set([
      ...yards.map((port) => port.id),
      ...quotes.map((quote) => quote.port_id),
    ]),
  ].sort();
  const classOptions = [
    ...new Set([
      ...(catalog?.ship_classes.map((item) => item.id) ?? []),
      ...quotes.map((quote) => quote.ship_class),
    ]),
  ].sort();
  const ships = fleet(account);
  const pages = Math.max(1, Math.ceil(filtered.length / 6));
  const currentPage = Math.min(page, pages - 1);

  return (
    <div className={styles.workspace}>
      <Section title="Build quotes" icon={Hammer}>
        <p className={styles.note}>
          Recorded quotes are specific to each yard. A quote is not a
          commissioned order.
        </p>
        <div className={styles.toolbar}>
          <label>
            Shipyard
            <select
              value={portFilter}
              onChange={(event) => {
                setPortFilter(event.target.value);
                setPage(0);
              }}
            >
              <option value="">All recorded shipyards</option>
              {portOptions.map((id) => (
                <option key={id} value={id}>
                  {catalog?.ports.find((port) => port.id === id)?.name ??
                    humanize(id)}
                </option>
              ))}
            </select>
          </label>
          <label>
            Hull class
            <select
              value={classFilter}
              onChange={(event) => {
                setClassFilter(event.target.value);
                setPage(0);
              }}
            >
              <option value="">All classes</option>
              {classOptions.map((id) => (
                <option key={id} value={id}>
                  {catalog?.ship_classes.find((item) => item.id === id)?.name ??
                    humanize(id)}
                </option>
              ))}
            </select>
          </label>
          <label>
            Materials
            <select
              value={availability}
              onChange={(event) => {
                setAvailability(event.target.value);
                setPage(0);
              }}
            >
              <option value="">Any availability</option>
              <option value="true">Available</option>
              <option value="false">Shortfall</option>
            </select>
          </label>
          {(portFilter || classFilter || availability) && (
            <button
              type="button"
              className={styles.reset}
              onClick={() => {
                setPortFilter("");
                setClassFilter("");
                setAvailability("");
                setPage(0);
              }}
            >
              Clear filters
            </button>
          )}
        </div>
        <Dataset
          account={account}
          name="yard_quotes"
          emptyTitle="No build quotes recorded"
          emptyDescription="Build prices cannot be reconstructed from catalog labour alone."
        >
          {filtered.length ? (
            <>
              <div className={styles.cardGrid}>
                {filtered
                  .slice(currentPage * 6, currentPage * 6 + 6)
                  .map((quote) => (
                    <article
                      className={styles.quoteCard}
                      key={quote.observationKey}
                    >
                      <div className={styles.cardLabel}>
                        <Hammer
                          size={17}
                          strokeWidth={1.7}
                          aria-hidden="true"
                        />
                        <h3>
                          {catalog?.ship_classes.find(
                            (item) => item.id === quote.ship_class,
                          )?.name ?? humanize(quote.ship_class)}
                        </h3>
                      </div>
                      <p className={styles.iconLabel}>
                        <MapPin
                          size={15}
                          strokeWidth={1.7}
                          aria-hidden="true"
                        />
                        {catalog?.ports.find(
                          (port) => port.id === quote.port_id,
                        )?.name ?? humanize(quote.port_id)}
                      </p>
                      <div className={styles.context}>
                        <Badge
                          icon={
                            quote.available === undefined
                              ? Package
                              : quote.available
                                ? CheckCircle2
                                : AlertTriangle
                          }
                          tone={
                            quote.available === undefined
                              ? "neutral"
                              : quote.available
                                ? "good"
                                : "warning"
                          }
                        >
                          {quote.available === undefined
                            ? "Availability not recorded"
                            : quote.available
                              ? "Materials available"
                              : "Material shortfall"}
                        </Badge>
                        <span>{humanize(quote.build_mode)}</span>
                      </div>
                      <p className={styles.balance}>
                        {money(quote.total_cost_pence)}
                      </p>
                      {quote.total_cost_pence == null && (
                        <p className={styles.context}>
                          Materials not fully priced
                        </p>
                      )}
                      <DatasetStatus
                        account={account}
                        name={`yard_quotes/${quote.observationKey}`}
                      />
                      <details className={styles.inlineDetails}>
                        <summary>Costs and materials</summary>
                        <dl className={styles.details}>
                          <div>
                            <dt>Labour</dt>
                            <dd>{money(quote.labour_pence)}</dd>
                          </div>
                          <div>
                            <dt>Materials</dt>
                            <dd>{money(quote.materials_pence)}</dd>
                          </div>
                          <div>
                            <dt>Yard markup</dt>
                            <dd>
                              {money(quote.yard_markup_pence)} (
                              {percent(quote.yard_markup_bps)})
                            </dd>
                          </div>
                        </dl>
                        {quote.materials.length ? (
                          <ul className={styles.materialList}>
                            {quote.materials.map((material) => (
                              <li key={material.commodity_id}>
                                <Package
                                  size={13}
                                  strokeWidth={1.7}
                                  aria-hidden="true"
                                />
                                <span className={styles.primary}>
                                  {humanize(material.commodity_id)}
                                  <small>
                                    {count(material.quantity)} t needed,{" "}
                                    {material.in_stock === undefined
                                      ? "supply not recorded"
                                      : material.in_stock
                                        ? "in stock"
                                        : "shortfall"}
                                  </small>
                                </span>
                                <strong>{money(material.cost_pence)}</strong>
                              </li>
                            ))}
                          </ul>
                        ) : (
                          <p className={styles.note}>
                            No material requirements recorded.
                          </p>
                        )}
                      </details>
                    </article>
                  ))}
              </div>
              <nav className={styles.pagination} aria-label="Quote pages">
                <span aria-live="polite">
                  {count(filtered.length)} quotes, page {currentPage + 1} of{" "}
                  {pages}
                </span>
                <button
                  type="button"
                  className="icon-button"
                  aria-label="Previous quotes"
                  disabled={currentPage === 0}
                  onClick={() => setPage(currentPage - 1)}
                >
                  <ChevronLeft size={15} strokeWidth={1.7} aria-hidden="true" />
                </button>
                <button
                  type="button"
                  className="icon-button"
                  aria-label="Next quotes"
                  disabled={currentPage === pages - 1}
                  onClick={() => setPage(currentPage + 1)}
                >
                  <ChevronRight
                    size={15}
                    strokeWidth={1.7}
                    aria-hidden="true"
                  />
                </button>
              </nav>
            </>
          ) : (
            <EmptyState
              icon={Hammer}
              title="No matching build quotes"
              description="No recorded quotes match the selected yard, hull and material filters."
            />
          )}
        </Dataset>
      </Section>
      <Section title="Hull classes" icon={Ship}>
        <Dataset account={account} name="catalog">
          <DataTable
            label="Catalog hull classes"
            data={catalog?.ship_classes ?? []}
            rowKey={(shipClass) => shipClass.id}
            emptyIcon={Ship}
            columns={[
              {
                label: "Hull",
                value: (shipClass) => shipClass.name,
                render: (shipClass) => (
                  <span className={styles.primary}>
                    <span className={styles.iconLabel}>
                      <Ship size={13} strokeWidth={1.7} aria-hidden="true" />
                      {shipClass.name}
                    </span>
                    <small>{humanize(shipClass.hull_category)}</small>
                  </span>
                ),
              },
              {
                label: "Hold",
                value: (shipClass) => shipClass.hold_capacity_tons,
                render: (shipClass) =>
                  `${count(shipClass.hold_capacity_tons)} t`,
                className: styles.numeric,
              },
              {
                label: "Base speed",
                value: (shipClass) => shipClass.base_speed_centiknots,
                render: (shipClass) =>
                  `${count(shipClass.base_speed_centiknots / 100)} kn`,
                className: styles.numeric,
              },
              {
                label: "Phase",
                value: (shipClass) => shipClass.phase,
                render: (shipClass) => (
                  <span className={styles.primary}>
                    Phase {count(shipClass.phase)}
                    <small>
                      {clock
                        ? shipClass.phase > clock.unlocked_phase
                          ? "Locked"
                          : "Unlocked"
                        : "World phase not recorded"}
                    </small>
                  </span>
                ),
              },
              {
                label: "Requirements",
                render: (shipClass) => (
                  <details className={styles.inlineDetails}>
                    <summary>Hull requirements</summary>
                    <dl className={styles.details}>
                      <div>
                        <dt>Crew</dt>
                        <dd>{count(shipClass.crew_required)}</dd>
                      </div>
                      <div>
                        <dt>Maximum guns</dt>
                        <dd>{count(shipClass.max_cannons)}</dd>
                      </div>
                      <div>
                        <dt>Catalog labour only</dt>
                        <dd>{money(shipClass.base_labor_cost_pence)}</dd>
                      </div>
                    </dl>
                    <ul className={styles.materialList}>
                      {shipClass.build_materials.map((material) => (
                        <li key={material.commodity_id}>
                          <Package
                            size={13}
                            strokeWidth={1.7}
                            aria-hidden="true"
                          />
                          <span>{humanize(material.commodity_id)}</span>
                          <strong>{count(material.quantity)} t</strong>
                        </li>
                      ))}
                    </ul>
                  </details>
                ),
              },
            ]}
          />
          <p className={styles.note}>
            Hull ratings must cover every sea zone on a route. Catalog
            requirements do not establish market availability or payback.
          </p>
        </Dataset>
        <details className={styles.disclosure}>
          <summary>
            <Anchor size={15} strokeWidth={1.7} aria-hidden="true" />
            Shipyard ports
          </summary>
          <Dataset account={account} name="catalog">
            <DataTable
              label="Ports with shipyards"
              data={yards}
              rowKey={(port) => port.id}
              columns={[
                {
                  label: "Port",
                  value: (port) => port.name,
                  render: (port) => port.name,
                },
                {
                  label: "Region",
                  value: (port) => port.banking_region,
                  render: (port) => humanize(port.banking_region),
                },
                {
                  label: "Phase",
                  value: (port) => port.phase,
                  render: (port) => count(port.phase),
                  className: styles.numeric,
                },
                {
                  label: "Berths",
                  value: (port) => port.effective_berths,
                  render: (port) => count(port.effective_berths),
                  className: styles.numeric,
                },
                {
                  label: "Recorded quotes",
                  value: (port) =>
                    quotes.filter((quote) => quote.port_id === port.id).length,
                  render: (port) =>
                    count(
                      quotes.filter((quote) => quote.port_id === port.id)
                        .length,
                    ),
                  className: styles.numeric,
                },
              ]}
            />
          </Dataset>
        </details>
        <details className={styles.disclosure}>
          <summary>
            <Ship size={15} strokeWidth={1.7} aria-hidden="true" />
            Fleet and construction records
          </summary>
          <p className={styles.note}>
            No separate construction schedule is recorded. Fleet statuses do not
            establish an order or build commitment.
          </p>
          <Dataset account={account} name="ships">
            <DataTable
              label="Fleet records alongside shipyard quotes"
              data={ships}
              rowKey={(item) => item.ship.id}
              emptyIcon={Ship}
              columns={[
                {
                  label: "Ship",
                  value: (item) => item.ship.name,
                  render: (item) => (
                    <span className={styles.primary}>
                      <strong>{item.ship.name}</strong>
                      <small>{humanize(item.ship.ship_class)}</small>
                    </span>
                  ),
                },
                {
                  label: "Status",
                  value: (item) => item.ship.status,
                  render: (item) => <StatusBadge status={item.ship.status} />,
                },
                {
                  label: "Port",
                  value: (item) => item.ship.current_port_id ?? undefined,
                  render: (item) => humanize(item.ship.current_port_id),
                },
                {
                  label: "Details",
                  render: (item) => (
                    <details className={styles.inlineDetails}>
                      <summary>Ship record</summary>
                      <dl className={styles.details}>
                        <div>
                          <dt>Ship ID</dt>
                          <dd>{item.ship.id}</dd>
                        </div>
                        <div>
                          <dt>Created</dt>
                          <dd>{dateTime(item.ship.created_at)}</dd>
                        </div>
                        <div>
                          <dt>Updated</dt>
                          <dd>{dateTime(item.ship.updated_at)}</dd>
                        </div>
                      </dl>
                      <DatasetStatus
                        account={account}
                        name={shipSource(account, item.ship.id)}
                      />
                    </details>
                  ),
                },
              ]}
            />
          </Dataset>
        </details>
      </Section>
    </div>
  );
}
