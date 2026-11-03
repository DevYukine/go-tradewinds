"use client";

import { useMemo } from "react";
import { useNow } from "@/app/providers";
import { shipSource } from "@/lib/data";
import { buildDecisionView } from "@/lib/decisions";
import { count, dateTime, humanize, money, quantity } from "@/lib/format";
import {
  passengerHold,
  shipSummary,
  type ShipSummary,
} from "@/lib/ship-summary";
import { catalogPlaceName } from "@/lib/operations";
import type { Account, Catalog, Decision, ShipDetail } from "@/lib/types";
import { DecisionEconomics } from "./decisions";
import { ShipCurrentHold } from "./operations-overview";
import { ShipLossDetails } from "./ship-loss-details";
import { DatasetStatus, SpriteImage, StatusBadge } from "./ui";
import styles from "./fleet.module.css";

export function ShipInspector({
  account,
  detail,
  catalog,
  decisions,
  receivedAt = 0,
  summary: recordedSummary,
}: {
  account: Account;
  detail: ShipDetail;
  catalog?: Catalog;
  decisions?: Decision[];
  receivedAt?: number;
  summary?: ShipSummary;
}) {
  const now = Math.max(useNow(), receivedAt);
  const summary = useMemo(
    () =>
      recordedSummary ??
      shipSummary(account, detail, decisions ?? [], now, catalog),
    [recordedSummary, account, detail, decisions, now, catalog],
  );
  const { gone, operation, passengers, plan, decision } = summary;
  const decisionView = decision
    ? buildDecisionView(decision.details)
    : undefined;
  const hasEconomics = Boolean(
    decisionView &&
    (decisionView.cashRows.length ||
      decisionView.stressRows.length ||
      [
        decisionView.expectedNetPence,
        decisionView.gameDays,
        decisionView.dailyPence,
        decisionView.ballastRepeatGameDays,
        decisionView.ballastRepeatNetPence,
        decisionView.haircutBps,
      ].some((value) => value !== undefined)),
  );
  const observedCatalog = summary.catalog;
  const place = (id: string | null | undefined) =>
    catalogPlaceName(observedCatalog, id);
  const shipClass = observedCatalog?.ship_classes.find(
    (item) => item.id === detail.ship.ship_class,
  );
  const destinations = new Map<string, number>();
  for (const group of passengers.groups ?? []) {
    destinations.set(
      group.dest_port_id,
      (destinations.get(group.dest_port_id) ?? 0) + group.size,
    );
  }
  const cargoCapacity =
    typeof detail.cargo_used_tons === "number" &&
    Number.isFinite(detail.cargo_used_tons) &&
    typeof detail.effective_hold_capacity_tons === "number" &&
    Number.isFinite(detail.effective_hold_capacity_tons);

  return (
    <div className={styles.inspectorBody}>
      <div className={styles.shipIdentity}>
        <SpriteImage name={detail.ship.name} />
        <div>
          <strong>{detail.ship.name}</strong>
          <span className={styles.identity} data-gone={gone}>
            {summary.action}
          </span>
          {summary.eta && (
            <span className={styles.identity}>{summary.eta}</span>
          )}
        </div>
      </div>
      {summary.why.text && (
        <p
          className={
            summary.why.attention ? styles.warning : styles.summaryReason
          }
        >
          {summary.why.label}: {summary.why.text}
        </p>
      )}
      {gone ? (
        <section className={styles.inspectorSection}>
          <h3>Last afloat</h3>
          <p className={styles.note}>
            {summary.cargo.replace(/^Last afloat: /, "")}
          </p>
          <p className={styles.note}>
            {operation.route.replace(/^Last afloat: /, "")}
          </p>
        </section>
      ) : (
        <>
          <section className={styles.inspectorSection}>
            <h3>Cargo manifest</h3>
            {summary.cargoItems ? (
              summary.cargoItems.length ? (
                <ul className={styles.cargo}>
                  {summary.cargoItems.map(({ id, name, tons }) => (
                    <li key={id}>
                      <span>{name}</span>
                      <strong>{quantity(tons, "t")}</strong>
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
              {cargoCapacity
                ? `${count(detail.cargo_used_tons)} / ${quantity(detail.effective_hold_capacity_tons, "t")} of effective cargo capacity.`
                : "Cargo capacity use not recorded."}
            </p>
          </section>
          <section className={styles.inspectorSection}>
            <h3>People</h3>
            <dl className={styles.facts}>
              <div>
                <dt>Crew required</dt>
                <dd>{count(summary.crew)}</dd>
              </div>
              <div>
                <dt>
                  {passengers.text.startsWith("Last recorded")
                    ? "Last recorded passengers"
                    : "Passengers"}
                </dt>
                <dd>
                  {passengers.count === undefined
                    ? passengerHold(detail)
                      ? "Recorded; count unavailable"
                      : "Not recorded"
                    : count(passengers.count)}
                </dd>
              </div>
            </dl>
            {destinations.size > 0 && (
              <ul
                className={styles.passengers}
                aria-label="Passengers by destination"
              >
                {[...destinations]
                  .sort(([left], [right]) =>
                    place(left).localeCompare(place(right)),
                  )
                  .map(([id, size]) => (
                    <li key={id}>
                      <span>To {place(id)}</span>
                      <strong>
                        {count(size)} {size === 1 ? "passenger" : "passengers"}
                      </strong>
                    </li>
                  ))}
              </ul>
            )}
            {passengers.note && (
              <p className={styles.note}>{passengers.note}</p>
            )}
          </section>
          <section className={styles.inspectorSection}>
            <h3>Stores</h3>
            <dl className={styles.facts}>
              <div>
                <dt>Provisions aboard</dt>
                <dd>{quantity(detail.stores?.provisions_tons, "t")}</dd>
              </div>
              <div>
                <dt>Sea endurance</dt>
                <dd>
                  {quantity(
                    detail.stores?.sea_days_remaining,
                    "game days at sea",
                  )}
                </dd>
              </div>
            </dl>
          </section>
        </>
      )}
      <details className={styles.inspectorDisclosures}>
        <summary>More details</summary>
        <div className={styles.diagnosticSections}>
          {!gone && (
            <section className={styles.inspectorSection}>
              <h3>Plan and recorded holds</h3>
              {account.missions_unavailable ? (
                <p className={styles.note}>Active plan unavailable.</p>
              ) : plan?.source === "mission" ? (
                <>
                  <dl className={styles.facts}>
                    <div>
                      <dt>Active plan</dt>
                      <dd>{humanize(plan.kind)}</dd>
                    </div>
                    <div>
                      <dt>Plan phase</dt>
                      <dd>{humanize(plan.phase)}</dd>
                    </div>
                    <div>
                      <dt>Origin</dt>
                      <dd>{place(plan.origin)}</dd>
                    </div>
                    <div>
                      <dt>Destination</dt>
                      <dd>{place(plan.destination)}</dd>
                    </div>
                    {plan.commodity && (
                      <div>
                        <dt>Planned cargo</dt>
                        <dd>
                          {plan.quantity === undefined
                            ? "Quantity not recorded"
                            : `${count(plan.quantity)} t`}{" "}
                          {observedCatalog?.commodities.find(
                            (commodity) => commodity.id === plan.commodity,
                          )?.name ?? humanize(plan.commodity)}
                        </dd>
                      </div>
                    )}
                    <div>
                      <dt>Modeled contribution</dt>
                      <dd>{money(plan.expectedNetPence)}</dd>
                    </div>
                    <div>
                      <dt>Plan updated</dt>
                      <dd>{dateTime(plan.updatedAt)}</dd>
                    </div>
                  </dl>
                  <p className={styles.note}>
                    Modeled contribution is not settled earnings.
                  </p>
                </>
              ) : (
                <p className={styles.note}>No active plan recorded.</p>
              )}
              {(!operation.holdsCurrent || operation.holds.length > 0) && (
                <ShipCurrentHold operation={operation} />
              )}
            </section>
          )}
          <ShipLossDetails
            detail={detail}
            catalog={observedCatalog}
            historyUnavailable={
              account.ship_losses_unavailable ||
              account.ship_losses === undefined
            }
            className={styles.inspectorSection}
            factsClassName={styles.facts}
            noteClassName={styles.note}
          />
          {!gone && (
            <>
              {passengers.groups && passengers.groups.length > 0 && (
                <section className={styles.inspectorSection}>
                  <h3>Passenger groups and promises</h3>
                  {passengers.groups.map((group, index) => (
                    <dl className={styles.facts} key={`${group.id}:${index}`}>
                      <div>
                        <dt>
                          {count(group.size)}{" "}
                          {humanize(group.kind).toLowerCase()}
                        </dt>
                        <dd>
                          {place(group.origin_port_id)} to{" "}
                          {place(group.dest_port_id)}
                        </dd>
                      </div>
                      <div>
                        <dt>Agreed fare per person</dt>
                        <dd>{money(group.fare_per_head_pence)}</dd>
                      </div>
                      <div>
                        <dt>Full fare if on time</dt>
                        <dd>{money(group.full_fare_pence)}</dd>
                      </div>
                      <div>
                        <dt>Boarded</dt>
                        <dd>{dateTime(group.boarded_at)}</dd>
                      </div>
                      <div>
                        <dt>Promised arrival</dt>
                        <dd>{dateTime(group.promised_arrival_at)}</dd>
                      </div>
                    </dl>
                  ))}
                  <p className={styles.note}>
                    Agreed fares are not earned cash; lateness can reduce
                    payment.
                  </p>
                </section>
              )}
              <section className={styles.inspectorSection}>
                <h3>Recorded voyage</h3>
                <dl className={styles.facts}>
                  <div>
                    <dt>Current port</dt>
                    <dd>{place(detail.ship.current_port_id)}</dd>
                  </div>
                  <div>
                    <dt>Destination</dt>
                    <dd>
                      {detail.ship.status === "sailing"
                        ? place(detail.route_legs?.at(-1)?.to_node)
                        : "No active voyage recorded"}
                    </dd>
                  </div>
                  <div>
                    <dt>Recorded current leg</dt>
                    <dd>
                      {detail.ship.status === "sailing" &&
                      detail.ship.current_leg_idx !== null &&
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
                        ? quantity(detail.voyage.speed_centiknots / 100, "kn")
                        : "Not recorded"}
                    </dd>
                  </div>
                  <div>
                    <dt>Starving legs</dt>
                    <dd>
                      {detail.voyage?.legs
                        ? detail.voyage.legs
                            .filter((leg) => leg.starving)
                            .map((leg) => leg.leg_idx + 1)
                            .join(", ") || "None in recorded estimate"
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
                          {place(leg.from_node)} to {place(leg.to_node)}
                          <small>
                            {quantity(leg.distance_nm, "nm")},{" "}
                            {humanize(leg.edge_class)}
                            {leg.is_sound_toll ? ", Sound toll" : ""}
                          </small>
                          {estimate && (
                            <small>
                              {quantity(estimate.speed_centiknots / 100, "kn")};
                              ETA {dateTime(estimate.estimated_arrival_at)}
                              {estimate.starving ? "; starving" : ""}
                            </small>
                          )}
                        </li>
                      );
                    })}
                  </ol>
                )}
              </section>
              <section className={styles.inspectorSection}>
                <h3>Berth and turnaround</h3>
                <dl className={styles.facts}>
                  <div>
                    <dt>Berthed at</dt>
                    <dd>
                      {detail.ship.berthed_at === null
                        ? "No berth recorded"
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
                  <div>
                    <dt>Exact provision units</dt>
                    <dd>{count(detail.ship.stores_provisions_units)}</dd>
                  </div>
                  <div>
                    <dt>Passenger space</dt>
                    <dd>{quantity(passengerHold(detail), "t")}</dd>
                  </div>
                </dl>
              </section>
            </>
          )}
          <section className={styles.inspectorSection}>
            <h3>Hull and cannons</h3>
            <dl className={styles.facts}>
              <div>
                <dt>Ship class</dt>
                <dd>{shipClass?.name ?? humanize(detail.ship.ship_class)}</dd>
              </div>
              <div>
                <dt>Hull rating</dt>
                <dd>{humanize(shipClass?.hull_category)}</dd>
              </div>
              <div>
                <dt>Hull hold capacity</dt>
                <dd>{quantity(detail.hold_capacity_tons, "t")}</dd>
              </div>
              <div>
                <dt>Mounted cannons</dt>
                <dd>
                  {detail.ship.cannons_count === null
                    ? "Not recorded"
                    : shipClass
                      ? `${count(detail.ship.cannons_count)} / ${count(shipClass.max_cannons)}`
                      : count(detail.ship.cannons_count)}
                </dd>
              </div>
              <div>
                <dt>Base catalog speed</dt>
                <dd>
                  {shipClass
                    ? quantity(shipClass.base_speed_centiknots / 100, "kn")
                    : "Not recorded"}
                </dd>
              </div>
              <div>
                <dt>Created</dt>
                <dd>{dateTime(detail.ship.created_at)}</dd>
              </div>
            </dl>
          </section>
          {detail.former_names && detail.former_names.length > 0 && (
            <section className={styles.inspectorSection}>
              <h3>Former names</h3>
              <dl className={styles.facts}>
                {detail.former_names.map((former, index) => (
                  <div key={`${former.renamed_at}:${index}`}>
                    <dt>{former.name}</dt>
                    <dd>Dropped {dateTime(former.renamed_at)}</dd>
                  </div>
                ))}
              </dl>
            </section>
          )}
          <section className={styles.inspectorSection}>
            <h3>Latest recorded decision</h3>
            {decision ? (
              <>
                <StatusBadge status={decision.kind} />
                <p className={styles.note}>{dateTime(decision.created_at)}</p>
                <p className={styles.decisionReason}>
                  {decision.reason.trim() || "Reason not recorded."}
                </p>
                <p className={styles.note}>
                  Historical evaluation, not a guaranteed next action.
                </p>
                {decisionView?.notPriced ? (
                  <p className={styles.note}>
                    Rejected before valuation; economics not recorded.
                  </p>
                ) : hasEconomics && decisionView ? (
                  <DecisionEconomics view={decisionView} />
                ) : (
                  <p className={styles.note}>Economics not recorded.</p>
                )}
              </>
            ) : (
              <p className={styles.note}>
                No decision for this ship in the loaded records.
              </p>
            )}
          </section>
          {!gone && (
            <section className={styles.inspectorSection}>
              <h3>Recording sources</h3>
              <h4 className={styles.note}>Ship observation</h4>
              <DatasetStatus
                account={account}
                name={shipSource(account, detail.ship.id)}
                expanded
                receivedAt={receivedAt}
              />
              <h4 className={styles.note}>Controller observation</h4>
              <DatasetStatus
                account={account}
                name="_engine"
                expanded
                receivedAt={receivedAt}
              />
              {passengers.groups && (
                <>
                  <h4 className={styles.note}>Passengers aboard</h4>
                  <DatasetStatus
                    account={account}
                    name={`passenger_aboard/${detail.ship.id}`}
                    expanded
                    receivedAt={receivedAt}
                  />
                </>
              )}
              <p className={styles.note}>
                Schedules use real UTC. Stores are separate from trade cargo;
                sea endurance uses game time. Crew required is a class
                requirement, not an observed roster.
              </p>
            </section>
          )}
        </div>
      </details>
    </div>
  );
}
