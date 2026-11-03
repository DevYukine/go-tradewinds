"use client";

import { Banknote, CheckCircle2, FileClock } from "lucide-react";
import { useMemo } from "react";
import { useNow } from "@/app/providers";
import { dataset, number, useLoans } from "@/lib/data";
import { age, count, dateTime, humanize, money, percent } from "@/lib/format";
import {
  loanInstallments,
  loanObservationStale,
  loanTimestamp,
  paymentTiming,
  shortLoanID,
  summarizeLoans,
  type LoanRow,
} from "@/lib/loans";
import type {
  Account,
  CompanyProfile,
  LoanAccount,
  LoanEntry,
} from "@/lib/types";
import { Badge, DataTable, EmptyState, Section, type Column } from "./ui";
import operations from "./operations.module.css";
import styles from "./loans.module.css";

const ledgerDisclosure =
  "Loan rows show lifetime repayments. The retained operating ledger excludes principal repayments; interest coverage begins with recorded ledger backfill and may not match lifetime totals.";

export function LoansView({
  accountID,
  accounts,
}: {
  accountID: string;
  accounts: Account[];
}) {
  const query = useLoans(accountID);
  const now = useNow();
  const summary = useMemo(
    () => summarizeLoans(query.data?.accounts ?? []),
    [query.data],
  );
  const companies = useMemo(
    () =>
      new Map(
        accounts.map((account) => [
          account.company_id,
          dataset<CompanyProfile>(account, "profile")?.company.name,
        ]),
      ),
    [accounts],
  );
  const errorNotice = query.isError ? (
    <div className="notice notice-danger" role="alert">
      <strong>Loan record updates interrupted.</strong> {query.error.message}{" "}
      {query.data && "Last successful records remain visible. "}
      Reconnection is automatic; this dashboard does not contact the game.
    </div>
  ) : null;

  if (query.isPending) {
    return (
      <div className="loading-state" role="status">
        Loading recorded loan history…
      </div>
    );
  }
  if (!query.data) {
    return (
      <div className={styles.workspace}>
        {errorNotice}
        <EmptyState
          icon={Banknote}
          title="Loan records unavailable"
          description="The recording service has not returned loan history. No debt or repayment total can be established from this response."
        />
      </div>
    );
  }
  if (!query.data.accounts.length) {
    return (
      <div className={styles.workspace}>
        {errorNotice}
        <EmptyState
          icon={Banknote}
          title="No loan accounts recorded"
          description="Loan history will appear here when the recording service has a configured borrower account."
        />
      </div>
    );
  }

  const paidBack =
    summary.principalRepaidPence !== undefined &&
    summary.interestPaidPence !== undefined
      ? summary.principalRepaidPence + summary.interestPaidPence
      : undefined;
  const next = summary.nextPayment;
  const oldestObservation = summary.oldestObservation;
  const staleByAge = loanObservationStale(oldestObservation, now);
  const numeric = `${operations.numeric} ${styles.numberCell}`;

  return (
    <div className={`${operations.workspace} ${styles.workspace}`}>
      {errorNotice}
      <section aria-label="Loan essentials">
        <dl className={styles.essentials}>
          <div>
            <dt>Owed now</dt>
            <dd className={styles.owed}>{money(summary.owedPence)}</dd>
            <dd className={styles.context}>
              {count(summary.active.length)} active or defaulted{" "}
              {summary.active.length === 1 ? "loan" : "loans"}
              {summary.unobservedAccounts > 0 &&
                `; ${count(summary.unobservedAccounts)} ${summary.unobservedAccounts === 1 ? "account" : "accounts"} not observed`}
            </dd>
            {oldestObservation && (
              <dd className={styles.context}>
                Oldest debt observation
                <br />
                <time dateTime={oldestObservation}>
                  {dateTime(oldestObservation)}
                </time>
                <br />
                {age(oldestObservation, now)}
                {staleByAge && " / Stale by age"}
              </dd>
            )}
            {summary.defaultedLoans > 0 && (
              <dd className={styles.context}>
                {count(summary.defaultedLoans)} defaulted{" "}
                {summary.defaultedLoans === 1 ? "loan" : "loans"}; recovery is
                outside the regular schedule
              </dd>
            )}
          </div>
          <div>
            <dt>Paid back, loan lifetime</dt>
            <dd>{money(paidBack)}</dd>
            <dd className={styles.context}>
              Principal {money(summary.principalRepaidPence)}
              <br />
              Interest {money(summary.interestPaidPence)}
            </dd>
            <dd className={styles.context}>
              {summary.repaidCount === undefined
                ? "Repaid loan count not recorded"
                : `${count(summary.repaidCount)} fully repaid ${summary.repaidCount === 1 ? "loan" : "loans"}`}
            </dd>
          </div>
          <div>
            <dt>Scheduled installments left (derived)</dt>
            <dd>
              {summary.remainingInstallments === undefined
                ? "Unknown"
                : count(summary.remainingInstallments)}
            </dd>
            <dd className={styles.context}>
              {summary.remainingInstallments === undefined
                ? `${count(summary.knownInstallments)} known installments; complete count unavailable`
                : summary.remainingInstallments === 0
                  ? "No remaining scheduled installments recorded"
                  : "Derived from recorded payment schedules"}
              {summary.unknownInstallmentLoans > 0 && (
                <>
                  <br />
                  {count(summary.unknownInstallmentLoans)}{" "}
                  {summary.unknownInstallmentLoans === 1
                    ? "loan schedule"
                    : "loan schedules"}{" "}
                  unknown
                </>
              )}
            </dd>
          </div>
          <div>
            <dt>Earliest recorded next payment</dt>
            <dd>
              {next
                ? money(next.amountPence)
                : summary.defaultedLoans > 0 &&
                    summary.defaultedLoans === summary.active.length
                  ? "No regular schedule"
                  : summary.owedPence === 0
                    ? "None scheduled"
                    : "Not recorded"}
            </dd>
            {next && (
              <dd className={styles.context}>
                <strong>{paymentTiming(next.dueAt, now)}</strong>
                <br />
                <time dateTime={next.dueAt}>{dateTime(next.dueAt)}</time>
                <br />
                {next.rows.length > 1
                  ? `${count(next.rows.length)} loans due together`
                  : `${next.rows[0].account.label} / ${next.rows[0].loan.lender_name}`}
              </dd>
            )}
            {summary.unknownNextPaymentLoans > 0 && (
              <dd className={styles.context}>
                {count(summary.unknownNextPaymentLoans)} outstanding{" "}
                {summary.unknownNextPaymentLoans === 1
                  ? "loan has"
                  : "loans have"}{" "}
                no recorded next due date
              </dd>
            )}
          </div>
        </dl>
        {summary.arrearsPence > 0 && (
          <p className={operations.warning}>
            Recorded arrears: {money(summary.arrearsPence)}.{" "}
            {summary.arrearsDeadline && (
              <>
                Earliest grace deadline covers{" "}
                {money(summary.arrearsDeadline.amountPence)}:{" "}
                {paymentTiming(summary.arrearsDeadline.dueAt, now)} /{" "}
                <time dateTime={summary.arrearsDeadline.dueAt}>
                  {dateTime(summary.arrearsDeadline.dueAt)}
                </time>
                .{" "}
              </>
            )}
            {summary.unknownArrearsDeadlineLoans > 0 &&
              `${count(summary.unknownArrearsDeadlineLoans)} ${summary.unknownArrearsDeadlineLoans === 1 ? "arrears deadline is" : "arrears deadlines are"} not recorded. `}
            Arrears grace is separate from regular scheduled installments.
          </p>
        )}
        <p className={operations.note}>
          Borrower loans in the selected account scope. Payments are lifetime
          amounts from stored loan rows, not just closed loans or the recent
          ledger. Times are real UTC; installment counts use recorded game
          terms.
        </p>
        <p className={styles.reportTime} aria-busy={query.isFetching}>
          Report read {dateTime(query.data.updated_at)}.
          {query.isFetching && <span> Reading stored records…</span>}
        </p>
        {query.data.accounts.map((account) => (
          <LoanRecordNotice
            key={account.account_id}
            account={account}
            now={now}
          />
        ))}
      </section>

      <Section
        title="Active and defaulted loans"
        icon={Banknote}
        className={operations.section}
        actions={<Badge>{count(summary.active.length)} recorded</Badge>}
      >
        <p className={operations.note}>
          Owed now includes outstanding principal and accrued interest.
          Installments left are derived from the schedule, never by dividing
          debt by the installment amount. Defaulted or uncertain arrears
          schedules are shown as unknown; a grace deadline is not an extra
          installment.
        </p>
        <LoanTable
          label="Active and defaulted borrower loans"
          rows={summary.active}
          mode="active"
          companies={companies}
          now={now}
        />
      </Section>

      <Section
        title="Fully repaid loans"
        icon={CheckCircle2}
        className={operations.section}
        actions={
          <Badge tone="good">{count(summary.repaid.length)} recorded</Badge>
        }
      >
        <p className={operations.note}>
          Only loans recorded as repaid appear here. Lifetime paid amounts
          include principal and interest paid before closure.
        </p>
        <LoanTable
          label="Fully repaid borrower loans"
          rows={summary.repaid}
          mode="repaid"
          companies={companies}
          now={now}
        />
      </Section>

      {summary.otherClosed.length > 0 && (
        <Section
          title="Other closed loans"
          icon={FileClock}
          className={operations.section}
          actions={<Badge>{count(summary.otherClosed.length)} recorded</Badge>}
        >
          <p className={operations.note}>
            Recovered and written-off loans are not counted as fully repaid.
            Collateral credit, extinguished debt and write-offs are separate
            from principal and interest paid back.
          </p>
          <LoanTable
            label="Other closed borrower loans"
            rows={summary.otherClosed}
            mode="other"
            companies={companies}
            now={now}
          />
        </Section>
      )}

      <details className={styles.coverage}>
        <summary>Record coverage and payment cross-check</summary>
        <p className={operations.note}>{ledgerDisclosure}</p>
        <p className={operations.note}>
          Differences are loan-row lifetime payments minus retained ledger
          payments, in the selected account scope. The two sources are not added
          together. Matching totals do not establish full historical coverage.
        </p>
        <div className={styles.coverageAccounts}>
          {query.data.accounts.map((account) => (
            <section key={account.account_id}>
              <h3>{accountName(account, companies)}</h3>
              <dl className={operations.details}>
                <div>
                  <dt>Current debt observation</dt>
                  <dd>
                    {dateTime(account.observed_at)}
                    {account.observed_at &&
                      ` / ${age(account.observed_at, now)}`}
                    {(account.loans_stale ||
                      loanObservationStale(account.observed_at, now)) &&
                      " / Stale"}
                  </dd>
                </div>
                <div>
                  <dt>Full-history capture</dt>
                  <dd>
                    {account.history_complete
                      ? "Complete at last full capture"
                      : "Incomplete; recorded loans only"}
                  </dd>
                </div>
                <div>
                  <dt>Last successful full capture</dt>
                  <dd>{dateTime(account.history_refreshed_at)}</dd>
                </div>
                <div>
                  <dt>Last full-capture attempt</dt>
                  <dd>{dateTime(account.history_attempted_at)}</dd>
                </div>
              </dl>
            </section>
          ))}
        </div>
        <DataTable
          label="Loan-row lifetime payments and retained ledger cross-check"
          data={query.data.accounts}
          rowKey={(account) => account.account_id}
          columns={[
            {
              label: "Account and company",
              value: (account) => accountName(account, companies),
              render: (account) => accountName(account, companies),
            },
            {
              label: "Loan principal repaid",
              value: (account) => number(account.totals.principal_repaid_pence),
              render: (account) => money(account.totals.principal_repaid_pence),
              className: numeric,
            },
            {
              label: "Ledger principal repaid",
              value: (account) =>
                number(account.totals.ledger_principal_repaid_pence),
              render: (account) =>
                money(account.totals.ledger_principal_repaid_pence),
              className: numeric,
            },
            {
              label: "Principal difference",
              value: (account) =>
                number(account.totals.principal_difference_pence),
              render: (account) =>
                money(account.totals.principal_difference_pence),
              className: numeric,
            },
            {
              label: "Loan interest paid",
              value: (account) => number(account.totals.interest_paid_pence),
              render: (account) => money(account.totals.interest_paid_pence),
              className: numeric,
            },
            {
              label: "Ledger interest paid",
              value: (account) =>
                number(account.totals.ledger_interest_paid_pence),
              render: (account) =>
                money(account.totals.ledger_interest_paid_pence),
              className: numeric,
            },
            {
              label: "Interest difference",
              value: (account) =>
                number(account.totals.interest_difference_pence),
              render: (account) =>
                money(account.totals.interest_difference_pence),
              className: numeric,
            },
          ]}
        />
      </details>
    </div>
  );
}

function accountName(
  account: LoanAccount,
  companies: Map<string, string | undefined>,
) {
  const company = companies.get(account.company_id);
  return `${account.label} / ${company || `Company ${shortLoanID(account.company_id)}`}`;
}

function LoanRecordNotice({
  account,
  now,
}: {
  account: LoanAccount;
  now: number;
}) {
  const missing = loanTimestamp(account.observed_at) === undefined;
  const stale =
    account.loans_stale || loanObservationStale(account.observed_at, now);
  if (
    !missing &&
    !stale &&
    !account.loans_error &&
    account.history_complete &&
    !account.history_error &&
    !account.recording_error
  ) {
    return null;
  }
  return (
    <div
      className={`${operations.warning} ${styles.recordNotice}`}
      role="status"
    >
      <strong>{account.label}</strong>
      {missing ? (
        <p>No current loan observation is recorded. Amount owed is unknown.</p>
      ) : stale ? (
        <p>
          Loan records are stale. Last observed {dateTime(account.observed_at)}
          {" / "}
          {age(account.observed_at, now)}.
        </p>
      ) : null}
      {account.loans_error && (
        <p>Loan observation coverage: {account.loans_error}</p>
      )}
      {!account.history_complete && (
        <p>
          Full history is incomplete. Older loans, lifetime paid totals and the
          fully repaid count may be missing; displayed values cover recorded
          loans only.
        </p>
      )}
      {account.history_error && (
        <p>Full-history coverage: {account.history_error}</p>
      )}
      {account.recording_error && (
        <p>Loan recording: {account.recording_error}</p>
      )}
    </div>
  );
}

function LoanTable({
  label,
  rows,
  mode,
  companies,
  now,
}: {
  label: string;
  rows: LoanRow[];
  mode: "active" | "repaid" | "other";
  companies: Map<string, string | undefined>;
  now: number;
}) {
  const numeric = `${operations.numeric} ${styles.numberCell}`;
  const columns: Column<LoanRow>[] = [
    {
      label: "Loan",
      value: ({ loan }) => loan.id || undefined,
      className: styles.identityCell,
      render: ({ loan }) => (
        <div className={operations.primary}>
          <details className={styles.loanDetails}>
            <summary>{shortLoanID(loan.id)}</summary>
            <LoanTerms loan={loan} />
          </details>
          <small>
            {loan.is_mortgage ? "Mortgage" : humanize(loan.repayment)}
          </small>
        </div>
      ),
    },
    {
      label: "Account and company",
      value: ({ account }) => accountName(account, companies),
      render: ({ account }) => accountName(account, companies),
      className: styles.identityCell,
    },
    {
      label: "Lender",
      value: ({ loan }) =>
        loan.lender_name.trim() || loan.lender_company_id || undefined,
      render: ({ loan }) => (
        <span title={loan.lender_company_id}>{loan.lender_name}</span>
      ),
      className: styles.identityCell,
    },
    {
      label: "Status",
      value: ({ loan }) => loan.status || undefined,
      render: ({ loan }) => (
        <Badge
          tone={
            loan.status === "defaulted"
              ? "danger"
              : loan.status === "repaid"
                ? "good"
                : loan.status === "written_off"
                  ? "warning"
                  : "neutral"
          }
        >
          {humanize(loan.status)}
        </Badge>
      ),
    },
    {
      label: "Owed now",
      value: ({ loan }) => number(loan.owed_pence),
      render: ({ loan }) => money(loan.owed_pence),
      className: numeric,
    },
    {
      label: "Principal",
      value: ({ loan }) => number(loan.principal_pence),
      render: ({ loan }) => money(loan.principal_pence),
      className: numeric,
    },
    {
      label: "Principal repaid",
      value: ({ loan }) => number(loan.principal_repaid_pence),
      render: ({ loan }) => money(loan.principal_repaid_pence),
      className: numeric,
    },
    {
      label: "Interest paid",
      value: ({ loan }) => number(loan.interest_paid_pence),
      render: ({ loan }) => money(loan.interest_paid_pence),
      className: numeric,
    },
  ];
  if (mode === "active") {
    columns.push(
      {
        label: "Installment",
        value: ({ loan }) => number(loan.installment_pence),
        render: ({ loan }) => money(loan.installment_pence),
        className: numeric,
      },
      {
        label: "Next amount",
        value: ({ loan }) =>
          loan.status === "defaulted"
            ? undefined
            : number(loan.next_payment_pence),
        render: ({ loan }) =>
          loan.status === "defaulted"
            ? "Not scheduled"
            : money(loan.next_payment_pence),
        className: numeric,
      },
      {
        label: "Next payment due",
        value: ({ loan }) =>
          loan.status === "defaulted"
            ? undefined
            : loanTimestamp(loan.next_payment_due_at),
        render: ({ loan }) =>
          loan.status === "defaulted" ? (
            "Recovery outside regular schedule"
          ) : (
            <>
              {dateTime(loan.next_payment_due_at)}
              {loanTimestamp(loan.next_payment_due_at) !== undefined && (
                <small>{paymentTiming(loan.next_payment_due_at, now)}</small>
              )}
            </>
          ),
        className: styles.dateCell,
      },
      {
        label: "Arrears",
        value: ({ loan }) => number(loan.arrears_pence),
        render: ({ loan }) => (
          <span className={loan.arrears_pence > 0 ? styles.arrears : undefined}>
            {money(loan.arrears_pence)}
            {loan.arrears_pence > 0 && (
              <small>
                {loan.status === "defaulted"
                  ? "Defaulted debt; no regular grace schedule"
                  : loanTimestamp(loan.arrears_due_by) === undefined
                    ? "Grace deadline not recorded"
                    : `Grace deadline ${dateTime(loan.arrears_due_by)} / ${paymentTiming(loan.arrears_due_by, now)}`}
              </small>
            )}
          </span>
        ),
        className: `${numeric} ${styles.arrearsCell}`,
      },
      {
        label: "Maturity",
        value: ({ loan }) => loanTimestamp(loan.matures_at),
        render: ({ loan }) => dateTime(loan.matures_at),
        className: styles.dateCell,
      },
      {
        label: "Installments left (derived)",
        value: ({ loan }) => loanInstallments(loan),
        render: ({ loan }) => (
          <>
            <span
              aria-label={
                loan.remaining_installments_derived
                  ? `${count(loanInstallments(loan))} derived installments left`
                  : undefined
              }
            >
              {loanInstallments(loan) === undefined
                ? "Unknown"
                : count(loanInstallments(loan))}
            </span>
            <small>{loan.installment_note}</small>
          </>
        ),
        className: `${numeric} ${styles.installmentsCell}`,
      },
    );
  } else {
    if (mode === "other") {
      columns.push(
        {
          label: "Collateral credit",
          value: ({ loan }) => number(loan.collateral_credit_pence),
          render: ({ loan }) => money(loan.collateral_credit_pence),
          className: numeric,
        },
        {
          label: "Extinguished",
          value: ({ loan }) => number(loan.extinguished_pence),
          render: ({ loan }) => money(loan.extinguished_pence),
          className: numeric,
        },
        {
          label: "Written off",
          value: ({ loan }) => number(loan.written_off_pence),
          render: ({ loan }) => money(loan.written_off_pence),
          className: numeric,
        },
      );
    }
    columns.push({
      label: "Closed",
      value: ({ loan }) => loanTimestamp(loan.closed_at),
      render: ({ loan }) => dateTime(loan.closed_at),
      className: styles.dateCell,
    });
  }
  return (
    <DataTable
      label={label}
      data={rows}
      rowKey={({ account, loan }) => `${account.account_id}:${loan.id}`}
      columns={columns}
      empty={
        mode === "active"
          ? "No active or defaulted loans recorded"
          : mode === "repaid"
            ? "No fully repaid loans recorded"
            : "No other closed loans recorded"
      }
      emptyIcon={Banknote}
    />
  );
}

function LoanTerms({ loan }: { loan: LoanEntry }) {
  return (
    <dl className={`${operations.details} ${styles.loanTerms}`}>
      <div>
        <dt>Loan ID</dt>
        <dd>{loan.id}</dd>
      </div>
      <div>
        <dt>Lender company ID</dt>
        <dd>{loan.lender_company_id || "Not recorded"}</dd>
      </div>
      <div>
        <dt>Banking region</dt>
        <dd>{humanize(loan.banking_region)}</dd>
      </div>
      <div>
        <dt>Outstanding principal</dt>
        <dd>{money(loan.outstanding_principal_pence)}</dd>
      </div>
      <div>
        <dt>Accrued interest</dt>
        <dd>{money(loan.accrued_interest_pence)}</dd>
      </div>
      <div>
        <dt>Total repayable</dt>
        <dd>{money(loan.total_repayable_pence)}</dd>
      </div>
      <div>
        <dt>Repayment</dt>
        <dd>{humanize(loan.repayment)}</dd>
      </div>
      <div>
        <dt>Term</dt>
        <dd>{count(loan.term_game_days)} game days</dd>
      </div>
      <div>
        <dt>Interest rate</dt>
        <dd>
          {loan.rate_basis === "annual"
            ? `${percent(loan.annual_rate_bps)} per 360 game days`
            : loan.rate_basis === "daily"
              ? `${percent(loan.daily_rate_bps)} per game day`
              : "Not recorded"}
        </dd>
      </div>
      <div>
        <dt>Disbursed</dt>
        <dd>{dateTime(loan.disbursed_at)}</dd>
      </div>
      {loan.defaulted_at && (
        <div>
          <dt>Defaulted</dt>
          <dd>{dateTime(loan.defaulted_at)}</dd>
        </div>
      )}
      <div>
        <dt>Loan source updated</dt>
        <dd>{dateTime(loan.updated_at)}</dd>
      </div>
      <div>
        <dt>Recorded</dt>
        <dd>{dateTime(loan.observed_at)}</dd>
      </div>
    </dl>
  );
}
