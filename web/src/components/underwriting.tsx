"use client";

import { Shield } from "lucide-react";
import { dataset, metadata, number, record, rows, text } from "@/lib/data";
import { dateTime, humanize, money, percent } from "@/lib/format";
import type { Account } from "@/lib/types";
import { DataTable, Dataset, DatasetStatus, Section, StatusBadge } from "./ui";
import styles from "./operations.module.css";

export function UnderwritingLedger({ account }: { account: Account }) {
  const riskData = dataset(account, "underwriting_risk");
  const positionData = dataset(account, "underwriting_positions");
  const risk = record(riskData);
  const positions = rows(positionData);
  const fundingUnverified = positions.some(
    (row) => row.funding_evidence == null,
  );
  const unrecorded =
    riskData === undefined &&
    positionData === undefined &&
    !metadata(account, "underwriting_risk").error &&
    !metadata(account, "underwriting_positions").error;
  return (
    <Section title="Our underwriting" icon={Shield}>
      {unrecorded ? (
        <p className={styles.note}>
          Underwriting assessments and owned positions have not been recorded
          yet.
        </p>
      ) : (
        <>
          <DatasetStatus account={account} name="underwriting_risk" />
          <dl className={styles.details}>
            <div>
              <dt>Capital at risk</dt>
              <dd>{money(number(risk.outstanding_pence))}</dd>
            </div>
            <div>
              <dt>Settled net</dt>
              <dd>{money(number(risk.realized_net_pence))}</dd>
            </div>
            <div>
              <dt>Remaining pilot allowance</dt>
              <dd>
                {number(risk.initial_pence) === 0
                  ? "Not started"
                  : money(number(risk.remaining_pence))}
              </dd>
            </div>
          </dl>
          <p className={styles.note}>
            Collateral can be lost in full. Premium is an entitlement until
            settlement; returned collateral is not profit. Vault cash already
            excludes locked funds.
          </p>
          {fundingUnverified && (
            <p className={styles.note}>
              New underwriting is paused until the collateral debit is matched
              to an owned vault. Shipping remains available.
            </p>
          )}
          <Dataset account={account} name="underwriting_positions">
            {positions.length > 0 ? (
              <DataTable
                label="Our accepted underwriting positions"
                data={positions}
                rowKey={(row, index) => text(row.id, String(index))}
                empty="No recorded owned underwriting positions match these filters"
                emptyIcon={Shield}
                columns={[
                  {
                    label: "Voyage",
                    value: (row) =>
                      `${text(record(row.frozen_policy).origin_port_id, "")} ${text(record(row.frozen_policy).dest_port_id, "")}`,
                    render: (row) => {
                      const policy = record(row.frozen_policy);
                      return (
                        <span className={styles.primary}>
                          <span>
                            {humanize(text(policy.origin_port_id))} →{" "}
                            {humanize(text(policy.dest_port_id))}
                          </span>
                          <small>{percent(number(row.slice_bps))} share</small>
                        </span>
                      );
                    },
                  },
                  {
                    label: "Status",
                    value: (row) => text(row.status, "") || undefined,
                    render: (row) => <StatusBadge status={text(row.status)} />,
                  },
                  {
                    label: "Collateral",
                    value: (row) => number(row.collateral_pence),
                    render: (row) => money(number(row.collateral_pence)),
                    className: styles.numeric,
                  },
                  {
                    label: "Premium entitlement",
                    value: (row) => number(row.premium_pence),
                    render: (row) => money(number(row.premium_pence)),
                    className: styles.numeric,
                  },
                  {
                    label: "Settled net",
                    value: (row) => number(row.net_pence),
                    render: (row) =>
                      row.net_pence == null
                        ? "Not settled"
                        : money(number(row.net_pence)),
                    className: styles.numeric,
                  },
                  {
                    label: "Accepted",
                    value: (row) => text(row.accepted_at, "") || undefined,
                    render: (row) => dateTime(text(row.accepted_at)),
                  },
                ]}
              />
            ) : (
              <p className={styles.note}>
                No accepted underwriting positions recorded.
              </p>
            )}
          </Dataset>
        </>
      )}
    </Section>
  );
}
