"use client";

import { useNow } from "@/app/providers";
import { dataset, fleet, metadata } from "@/lib/data";
import { age, count, dateTime, money } from "@/lib/format";
import {
  companyMoney,
  controllerStatus,
  datasetObservation,
  observationLabel,
  shipOperation,
  type ShipOperation,
} from "@/lib/operations";
import type {
  Account,
  Clock,
  CompanyProfile,
  DashboardData,
} from "@/lib/types";
import { useUI } from "@/lib/ui-store";
import { Badge, EmptyState, SpriteImage, StatusBadge } from "./ui";
import styles from "./operations-overview.module.css";

export function OperationsOverview({
  accounts,
  dashboard,
}: {
  accounts: Account[];
  dashboard: DashboardData;
}) {
  const now = useNow();
  const selectShip = useUI((state) => state.selectShip);
  const selectedShipID = useUI((state) => state.selectedShipID);
  const selectedShipAccountID = useUI((state) => state.selectedShipAccountID);

  return (
    <div className={styles.overview}>
      {accounts.map((account) => {
        const profile = dataset<CompanyProfile>(account, "profile");
        const ships = fleet(account);
        const unobservedHolds = controllerStatus(account, now).holds.filter(
          (hold) =>
            hold.ship_id &&
            !ships.some((detail) => detail.ship.id === hold.ship_id),
        );
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
            <ControllerSummary account={account} />
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
            {ships.length ? (
              <ul className={styles.ships}>
                {ships.map((detail) => {
                  const operation = shipOperation(account, detail, now);
                  return (
                    <li className={styles.shipRow} key={detail.ship.id}>
                      <div className={styles.shipIdentity}>
                        <button
                          type="button"
                          className={styles.shipButton}
                          aria-pressed={
                            detail.ship.id === selectedShipID &&
                            account.id === selectedShipAccountID
                          }
                          aria-label={`Inspect ${detail.ship.name}, ${account.label}`}
                          onClick={() => selectShip(detail.ship.id, account.id)}
                        >
                          <SpriteImage name={detail.ship.name} />
                          <strong>{detail.ship.name}</strong>
                        </button>
                        {operation.observation.state !==
                          fleetObservation.state && (
                          <span className={styles.observation}>
                            {observationLabel(operation.observation.state)}
                            {operation.observation.timestamp &&
                              `, ${age(operation.observation.timestamp, now)}`}
                            .
                          </span>
                        )}
                      </div>
                      <div className={styles.activity}>
                        {operation.observation.current ? (
                          <StatusBadge status={detail.ship.status} />
                        ) : (
                          <Badge>Last recorded: {operation.activity}</Badge>
                        )}
                        <span>{operation.route}</span>
                        <span className={styles.cargo}>{operation.cargo}</span>
                        {operation.starving && (
                          <span className={styles.warning}>
                            Starvation in recorded voyage estimate.
                          </span>
                        )}
                      </div>
                      <ShipCurrentHold operation={operation} />
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
                <ul>
                  {unobservedHolds.map((hold, index) => (
                    <li key={`${hold.ship_id}:${index}`}>
                      Ship {hold.ship_id}, last recorded hold (not current):{" "}
                      {hold.reason}
                    </li>
                  ))}
                </ul>
              </div>
            )}
          </section>
        );
      })}
    </div>
  );
}

export function ControllerSummary({ account }: { account: Account }) {
  const now = useNow();
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
