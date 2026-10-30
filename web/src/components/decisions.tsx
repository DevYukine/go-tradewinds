"use client";

import {
  AlertTriangle,
  Bot,
  CheckCircle2,
  Clock3,
  Pause,
  Route,
} from "lucide-react";
import { useState } from "react";
import { useNow } from "@/app/providers";
import { buildDecisionView, type DecisionView } from "@/lib/decisions";
import { age, count, dateTime, humanize, money, percent } from "@/lib/format";
import { controllerStatus, shipOperation } from "@/lib/operations";
import type { Account, Decision, ShipDetail } from "@/lib/types";
import { DataTable, Section, SpriteImage, StatusBadge } from "./ui";
import { ControllerSummary, ShipCurrentHold } from "./operations-overview";
import styles from "./operations.module.css";
import viewStyles from "./world-views.module.css";

export function DecisionPanel({
  account,
  decisions,
  ships,
}: {
  account: Account;
  decisions: Decision[];
  ships: ShipDetail[];
}) {
  const [shipID, setShipID] = useState("");
  const [kind, setKind] = useState("");
  const now = useNow();
  const controller = controllerStatus(account, now);
  const sorted = decisions
    .map((decision) => ({
      ...decision,
      category: decision.kind.split(":")[0],
      view: buildDecisionView(decision.details),
    }))
    .sort(
      (left, right) =>
        Date.parse(right.created_at) - Date.parse(left.created_at),
    );
  const filtered = sorted.filter(
    (decision) =>
      (!shipID || decision.ship_id === shipID) &&
      (!kind || decision.category === kind),
  );
  const shipOptions = [
    ...new Set(decisions.map((decision) => decision.ship_id).filter(Boolean)),
  ];
  const kinds = [
    ...new Set(sorted.map((decision) => decision.category)),
  ].sort();
  const shipName = (id: string) =>
    ships.find((item) => item.ship.id === id)?.ship.name ??
    (id || "Company evaluation");

  return (
    <div className={styles.workspace}>
      <ControllerSummary account={account} />
      <dl className={styles.facts} aria-label="Ship-scoped automation holds">
        {ships.map((detail) => (
          <div key={detail.ship.id}>
            <dt>{detail.ship.name}</dt>
            <dd>
              <ShipCurrentHold
                operation={shipOperation(account, detail, now)}
              />
            </dd>
          </div>
        ))}
        {controller.holds
          .filter(
            (hold) =>
              hold.ship_id &&
              !ships.some((detail) => detail.ship.id === hold.ship_id),
          )
          .map((hold, index) => (
            <div key={`${hold.ship_id}:${index}`}>
              <dt>{shipName(hold.ship_id)}</dt>
              <dd>
                Last recorded ship hold; ship observation unavailable:{" "}
                {hold.reason}
              </dd>
            </div>
          ))}
      </dl>
      <Section title="Automation ledger" icon={Bot} className={styles.section}>
        <p className={styles.note}>
          Recorded evaluations are not realized profit. Destination models end
          at delivery; legacy models retain their original basis.
          {decisions.some((decision) => decision.id == null) &&
            " Record IDs were not provided for some evaluations."}
        </p>
        <div className={styles.toolbar}>
          <label>
            Ship
            <select
              value={shipID}
              onChange={(event) => setShipID(event.target.value)}
            >
              <option value="">All ships and company evaluations</option>
              {shipOptions.map((id) => (
                <option key={id} value={id}>
                  {shipName(id)}
                </option>
              ))}
            </select>
          </label>
          <label>
            Decision kind
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
          {(shipID || kind) && (
            <button
              type="button"
              className={styles.reset}
              onClick={() => {
                setShipID("");
                setKind("");
              }}
            >
              Clear decision filters
            </button>
          )}
        </div>
        <DataTable
          label="Recorded planner decisions and modeled economics"
          data={filtered}
          rowKey={(decision, index) =>
            decision.id == null ? `missing:${index}` : String(decision.id)
          }
          empty="No decisions recorded for these filters"
          columns={[
            {
              label: "Recorded",
              value: (decision) => decision.created_at || undefined,
              render: (decision) => (
                <span className={styles.primary}>
                  {dateTime(decision.created_at)}
                  <small>{age(decision.created_at, now)}</small>
                </span>
              ),
            },
            {
              label: "Ship / company",
              value: (decision) => shipName(decision.ship_id),
              render: (decision) => (
                <span className={viewStyles.inline}>
                  {decision.ship_id && (
                    <SpriteImage name={shipName(decision.ship_id)} />
                  )}
                  {shipName(decision.ship_id)}
                </span>
              ),
            },
            {
              label: "Decision kind",
              value: (decision) => decision.kind || undefined,
              render: (decision) => (
                <span className={styles.primary}>
                  <StatusBadge status={decision.category} />
                  {decision.kind.includes(":") && (
                    <small>
                      {humanize(
                        decision.kind
                          .slice(decision.category.length + 1)
                          .replaceAll(":", " / "),
                      )}
                    </small>
                  )}
                </span>
              ),
            },
            {
              label: "Reason",
              value: (decision) => decision.reason || undefined,
              render: (decision) => {
                const Icon =
                  decision.category === "rejected"
                    ? AlertTriangle
                    : decision.category === "held"
                      ? Pause
                      : decision.category === "hold"
                        ? Clock3
                        : ["ranked", "selected", "executed", "good"].includes(
                              decision.category,
                            )
                          ? CheckCircle2
                          : Route;
                return (
                  <span className={viewStyles.reason}>
                    <Icon size={13} strokeWidth={1.7} aria-hidden="true" />
                    {decision.reason || "Not recorded"}
                  </span>
                );
              },
            },
            {
              label: "Origin",
              value: (decision) => decision.view.origin || undefined,
              render: (decision) => humanize(decision.view.origin),
            },
            {
              label: "Modeled net",
              value: (decision) => decision.view.expectedNetPence,
              render: (decision) =>
                decision.view.notPriced ? (
                  <span>
                    Not priced: {decision.reason || "No reason recorded"}
                  </span>
                ) : (
                  <span className={styles.primary}>
                    {money(decision.view.expectedNetPence)}
                    <small>
                      {decision.view.netLabel} ({decision.view.basisLabel})
                    </small>
                  </span>
                ),
              className: styles.numeric,
            },
            {
              label: "Evaluation",
              render: (decision) => (
                <details>
                  <summary>Route and economics</summary>
                  <dl className={styles.details}>
                    <div>
                      <dt>Destination</dt>
                      <dd>{humanize(decision.view.destination)}</dd>
                    </div>
                    <div>
                      <dt>Commodity</dt>
                      <dd>{humanize(decision.view.commodity)}</dd>
                    </div>
                  </dl>
                  {decision.view.notPriced ? (
                    <p className={styles.note}>
                      Not priced: {decision.reason || "No reason recorded"}
                    </p>
                  ) : (
                    <Economics view={decision.view} />
                  )}
                </details>
              ),
            },
          ]}
        />
        <p className={styles.coverage}>
          {count(filtered.length)} matching evaluations in the returned records.
          Missing economics remain unavailable.
        </p>
      </Section>
    </div>
  );
}

function Economics({ view }: { view: DecisionView }) {
  const separateFunding = view.basis === "destination";
  const fundingRows = separateFunding
    ? view.cashRows.filter(
        (row) =>
          row.key === "store_cash_pence" || row.key === "collateral_pence",
      )
    : [];
  const operatingRows = separateFunding
    ? view.cashRows.filter(
        (row) =>
          row.key !== "store_cash_pence" && row.key !== "collateral_pence",
      )
    : view.cashRows;
  return (
    <div className={styles.model}>
      <section aria-label={`${view.basisLabel} economics`}>
        <h4>{view.basisLabel}</h4>
        <p className={styles.note}>{view.economicsNotice}</p>
        {view.cashRows.length === 0 && (
          <p className={styles.note}>Not recorded</p>
        )}
        <dl className={styles.details}>
          {operatingRows.map(({ key, label, pence }) => (
            <div key={key}>
              <dt>{label}</dt>
              <dd>{money(pence)}</dd>
            </div>
          ))}
          {view.gameDays !== undefined && (
            <div>
              <dt>{view.durationLabel}</dt>
              <dd>{count(view.gameDays)} game days</dd>
            </div>
          )}
          {view.dailyPence !== undefined && (
            <div>
              <dt>{view.dailyLabel}</dt>
              <dd>{money(view.dailyPence)}</dd>
            </div>
          )}
        </dl>
        {fundingRows.length > 0 && (
          <>
            <h4>Separate cash funding</h4>
            <dl className={styles.details}>
              {fundingRows.map(({ key, label, pence }) => (
                <div key={key}>
                  <dt>{label}</dt>
                  <dd>{money(pence)}</dd>
                </div>
              ))}
            </dl>
          </>
        )}
      </section>
      <section aria-label="Diagnostics-only stress scenarios">
        <h4>Stress diagnostics</h4>
        <p className={styles.note}>{view.stressNotice}</p>
        {view.stressRows.length === 0 && view.haircutBps === undefined ? (
          <p className={styles.note}>Not recorded</p>
        ) : (
          <dl className={styles.details}>
            {view.stressRows.map(({ key, label, pence }) => (
              <div key={key}>
                <dt>{label}</dt>
                <dd>{money(pence)}</dd>
              </div>
            ))}
            {view.haircutBps !== undefined && (
              <div>
                <dt>{view.haircutLabel}</dt>
                <dd>{percent(view.haircutBps)}</dd>
              </div>
            )}
          </dl>
        )}
      </section>
      {view.ballastRepeatGameDays !== undefined &&
        view.ballastRepeatNetPence !== undefined && (
          <section aria-label="Ballast repeat forecast">
            <h4>Ballast repeat forecast</h4>
            <p className={styles.note}>
              Includes the empty return and turnaround, not realized earnings.
            </p>
            <dl className={styles.details}>
              <div>
                <dt>Repeat duration</dt>
                <dd>{count(view.ballastRepeatGameDays)} game days</dd>
              </div>
              <div>
                <dt>Repeat contribution</dt>
                <dd>{money(view.ballastRepeatNetPence)}</dd>
              </div>
            </dl>
          </section>
        )}
    </div>
  );
}
