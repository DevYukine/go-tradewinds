"use client";

import { useMemo } from "react";
import { useNow } from "@/app/providers";
import { chartHoldLabel } from "@/lib/chart";
import { dataset, fleet, metadata } from "@/lib/data";
import { age, count, dateTime, money } from "@/lib/format";
import {
  companyMoney,
  controllerStatus,
  datasetObservation,
  observationLabel,
  type ShipOperation,
} from "@/lib/operations";
import { shipSummary } from "@/lib/ship-summary";
import type {
  Account,
  Catalog,
  Clock,
  CompanyProfile,
  DashboardData,
} from "@/lib/types";
import { useUI } from "@/lib/ui-store";
import { ShipLossHistoryNotice } from "./ship-loss-details";
import { Badge, EmptyState, SpriteImage } from "./ui";
import styles from "./operations-overview.module.css";

export function OperationsOverview({
  accounts,
  dashboard,
  receivedAt = 0,
  catalog,
  inspectorOpen = false,
  onInspect,
}: {
  accounts: Account[];
  dashboard: DashboardData;
  receivedAt?: number;
  catalog?: Catalog;
  inspectorOpen?: boolean;
  onInspect?: (trigger: HTMLButtonElement) => void;
}) {
  const now = Math.max(useNow(), receivedAt);
  const selectShip = useUI((state) => state.selectShip);
  const selectedShipID = useUI((state) => state.selectedShipID);
  const selectedShipAccountID = useUI((state) => state.selectedShipAccountID);
  const shipRows = useMemo(
    () =>
      new Map(
        accounts.map((account) => [
          account.id,
          fleet(account).map((detail) => ({
            detail,
            summary: shipSummary(
              account,
              detail,
              dashboard.decisions,
              now,
              catalog,
            ),
          })),
        ]),
      ),
    [accounts, dashboard.decisions, now, catalog],
  );

  return (
    <div className={styles.overview}>
      {accounts.map((account) => {
        const profile = dataset<CompanyProfile>(account, "profile");
        const ships = shipRows.get(account.id) ?? [];
        const unobservedHolds = controllerStatus(account, now).holds.filter(
          (hold) =>
            hold.ship_id &&
            !ships.some(({ detail }) => detail.ship.id === hold.ship_id),
        );
        const unobservedShipCount = new Set(
          unobservedHolds.map((hold) => hold.ship_id),
        ).size;
        const moneyView = companyMoney(account, dashboard);
        const cashObservation = datasetObservation(account, "vaults", now);
        const fleetObservation = datasetObservation(account, "ships", now);
        const leaderboardObservation = datasetObservation(
          account,
          "leaderboard",
          now,
        );
        return (
          <section
            className={styles.company}
            key={account.id}
            aria-label={`${profile?.company.name ?? account.label} operations`}
          >
            <header className={styles.companyHeading}>
              <h2>{profile?.company.name ?? "Company not recorded"}</h2>
              <span>{account.label}</span>
            </header>
            <ControllerSummary account={account} receivedAt={receivedAt} />
            <dl
              className={styles.moneyStrip}
              aria-label={`${account.label} money`}
            >
              <div>
                <dt>Recorded cash</dt>
                <dd>{money(moneyView.cashPence)}</dd>
                <small>
                  Excludes escrow.
                  {cashObservation.state !== "current" && (
                    <>
                      {" "}
                      {observationLabel(cashObservation.state)}
                      {cashObservation.timestamp &&
                        `, ${age(cashObservation.timestamp, now)}`}
                      .
                    </>
                  )}
                </small>
              </div>
              <div>
                <dt>Settled net, loaded sample</dt>
                <dd
                  className={
                    moneyView.settledNetPence === undefined ||
                    moneyView.settledNetPence === 0
                      ? undefined
                      : moneyView.settledNetPence < 0
                        ? "negative"
                        : "positive"
                  }
                >
                  {money(moneyView.settledNetPence)}
                </dd>
                <small>
                  {count(moneyView.knownSettlements)} known-cost settlements
                  {moneyView.unknownSettlements > 0 &&
                    `; ${count(moneyView.unknownSettlements)} costs unknown`}
                  . Not complete-period profit.
                </small>
              </div>
              <div>
                <dt>Leaderboard net worth</dt>
                <dd>{money(moneyView.netWorthPence)}</dd>
                <small>
                  {moneyView.leaderboard?.you &&
                  !moneyView.leaderboard.you.is_calling_company
                    ? `The ranked company is ${moneyView.leaderboard.you.company_name}, not this company.`
                    : moneyView.netWorthPence === undefined
                      ? "No official valuation recorded for this company."
                      : `Hourly valuation, not earnings. As of ${dateTime(moneyView.leaderboard?.refreshed_at)}.`}
                  {moneyView.netWorthPence !== undefined &&
                    leaderboardObservation.state !== "current" &&
                    ` ${observationLabel(leaderboardObservation.state)}.`}
                  {moneyView.leaderboard?.next_refresh_at &&
                    now > Date.parse(moneyView.leaderboard.next_refresh_at) &&
                    " A newer scheduled valuation has not been recorded."}
                </small>
              </div>
            </dl>
            <div className={styles.shipHeading}>
              <h3>Ships</h3>
              <span>
                {observationLabel(fleetObservation.state)}
                {fleetObservation.timestamp &&
                  `, ${age(fleetObservation.timestamp, now)}`}
                .
              </span>
            </div>
            <ShipLossHistoryNotice
              account={account}
              className={styles.observation}
            />
            {ships.length ? (
              <ul className={styles.ships}>
                {ships.map(({ detail, summary }) => {
                  const selected =
                    inspectorOpen &&
                    detail.ship.id === selectedShipID &&
                    account.id === selectedShipAccountID;
                  return (
                    <li className={styles.shipRow} key={detail.ship.id}>
                      <SpriteImage name={detail.ship.name} decorative />
                      <div className={styles.shipSummary}>
                        <div className={styles.shipIdentity}>
                          <button
                            type="button"
                            className={styles.shipButton}
                            aria-expanded={selected}
                            aria-controls={
                              selected ? "ship-inspector" : undefined
                            }
                            aria-label={`Inspect ${detail.ship.name}, ${account.label}`}
                            aria-haspopup="dialog"
                            onClick={(event) => {
                              selectShip(detail.ship.id, account.id);
                              onInspect?.(event.currentTarget);
                            }}
                          >
                            <strong>{detail.ship.name}</strong>
                          </button>
                          <span
                            className={styles.action}
                            data-attention={
                              summary.gone && summary.why.attention
                            }
                          >
                            {summary.action}
                          </span>
                          {summary.eta && (
                            <span className={styles.arrival}>
                              {summary.eta}
                            </span>
                          )}
                        </div>
                        <p className={styles.cargo}>{summary.cargo}</p>
                        {!summary.gone && (
                          <div className={styles.people}>
                            <span>
                              {summary.crew === undefined
                                ? "Crew requirement not recorded"
                                : `Crew required ${count(summary.crew)}`}
                            </span>
                            <span>{summary.passengers.text}</span>
                            <span className={styles.stores}>
                              {summary.stores}
                            </span>
                          </div>
                        )}
                        {summary.gone && (
                          <p className={styles.people}>
                            {summary.operation.route}
                          </p>
                        )}
                        <div className={styles.reasonLine}>
                          {summary.why.text && (
                            <p
                              className={
                                summary.why.attention
                                  ? styles.warning
                                  : styles.reason
                              }
                            >
                              <span>{summary.why.label}: </span>
                              {summary.why.text}
                            </p>
                          )}
                          <button
                            type="button"
                            className={styles.detailsButton}
                            aria-label={`More details about ${detail.ship.name}`}
                            aria-expanded={selected}
                            aria-controls={
                              selected ? "ship-inspector" : undefined
                            }
                            aria-haspopup="dialog"
                            onClick={(event) => {
                              selectShip(detail.ship.id, account.id);
                              onInspect?.(event.currentTarget);
                            }}
                          >
                            More details
                          </button>
                        </div>
                      </div>
                    </li>
                  );
                })}
              </ul>
            ) : (
              <EmptyState
                title={
                  fleetObservation.state === "current"
                    ? "No ships in the recorded fleet"
                    : "Fleet unavailable"
                }
                description={
                  fleetObservation.state === "current"
                    ? "The current fleet observation contains no ships."
                    : "A complete current fleet observation has not been recorded. No activity is assumed."
                }
              />
            )}
            {unobservedHolds.length > 0 && (
              <div className={styles.companyNotices}>
                <strong>Ship observations unavailable</strong>
                <p>
                  Last recorded holds for {count(unobservedShipCount)}{" "}
                  unobserved {unobservedShipCount === 1 ? "ship" : "ships"}, not
                  current:{" "}
                  {[
                    ...new Set(
                      unobservedHolds.map((hold) =>
                        chartHoldLabel(hold.reason),
                      ),
                    ),
                  ].join(", ")}
                  .
                </p>
              </div>
            )}
          </section>
        );
      })}
    </div>
  );
}

export function ControllerSummary({
  account,
  receivedAt = 0,
}: {
  account: Account;
  receivedAt?: number;
}) {
  const now = Math.max(useNow(), receivedAt);
  const controller = controllerStatus(account, now);
  const controllerError = metadata(account, "_engine").error;
  const clock = dataset<Clock>(account, "clock");
  const clockObservation = datasetObservation(account, "clock", now);
  const mode = !account.enabled
    ? "Automation disabled"
    : controller.mode === "observe"
      ? controller.observation.current
        ? "Observe only"
        : "Last recorded observe mode"
      : controller.mode === "execute"
        ? controller.observation.current
          ? "Execute mode"
          : "Last recorded execute mode"
        : "Automation mode not recorded";
  const overdue = Boolean(
    controller.nextCheckAt && now > Date.parse(controller.nextCheckAt),
  );

  return (
    <div className={styles.controller} aria-label="Recorded automation status">
      <div className={styles.controllerHeading}>
        <Badge
          tone={
            account.enabled &&
            controller.mode === "execute" &&
            controller.observation.current
              ? "blue"
              : "neutral"
          }
        >
          {mode}
        </Badge>
        {clock?.paused && (
          <Badge tone="warning">
            {clockObservation.current
              ? "World paused"
              : "World last recorded paused"}
          </Badge>
        )}
        {!controller.observation.current && (
          <span className={styles.observation}>
            Controller:{" "}
            {observationLabel(controller.observation.state).toLowerCase()}.
          </span>
        )}
      </div>
      {controllerError && (
        <p className={styles.companyNotices} role="status">
          {controllerError}
        </p>
      )}
      <dl className={styles.checks}>
        <div>
          <dt>Last completed check</dt>
          <dd>
            {dateTime(controller.observedAt)}
            {controller.observedAt && ` (${age(controller.observedAt, now)})`}
          </dd>
        </div>
        <div>
          <dt>Next check target</dt>
          <dd>
            {dateTime(controller.nextCheckAt)}
            {overdue ? "; target passed" : ""}
          </dd>
        </div>
      </dl>
      {controller.accountHolds.length > 0 && (
        <div className={styles.companyNotices}>
          <strong>
            {controller.observation.current
              ? "Company notices"
              : "Last recorded company notices (not current)"}
          </strong>
          <ul>
            {controller.accountHolds.map((hold, index) => (
              <li key={`${index}:${hold.reason}`}>{hold.reason}</li>
            ))}
          </ul>
        </div>
      )}
    </div>
  );
}

export function ShipCurrentHold({ operation }: { operation: ShipOperation }) {
  return (
    <div className={styles.hold}>
      <strong>
        {operation.holds.length
          ? operation.holdsCurrent
            ? "Current hold"
            : "Last recorded hold (not current)"
          : operation.holdsCurrent
            ? "No current hold recorded"
            : "Hold assessment unavailable"}
      </strong>
      {operation.holds.length ? (
        <ul>
          {operation.holds.map((hold, index) => (
            <li key={`${index}:${hold.reason}`}>{hold.reason}</li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}
