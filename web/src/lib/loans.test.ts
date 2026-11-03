import { describe, expect, it } from "vitest";
import {
  loanInstallments,
  loanObservationStale,
  loanTimestamp,
  paymentTiming,
  shortLoanID,
  summarizeLoans,
} from "./loans";
import type { LoanAccount, LoanEntry } from "./types";

const observedAt = "2026-11-03T10:00:00Z";

function loan(overrides: Partial<LoanEntry> = {}): LoanEntry {
  return {
    id: "loan-1",
    lender_company_id: "lender-company-1",
    borrower_company_id: "borrower-1",
    banking_region: "europe",
    principal_pence: 100_000,
    rate_basis: "annual",
    daily_rate_bps: 0,
    annual_rate_bps: 1_000,
    term_game_days: 30,
    repayment: "amortised",
    total_repayable_pence: 100_820,
    installment_pence: 33_607,
    next_payment_due_at: "2026-11-03T12:00:00Z",
    next_payment_pence: 33_607,
    arrears_pence: 0,
    arrears_due_by: null,
    is_mortgage: false,
    recourse: true,
    collateral_kind: "none",
    collateral_ship_id: null,
    collateral_port_id: null,
    collateral_commodity_id: null,
    collateral_tons: null,
    collateral_contract_id: null,
    collateral_deposit_id: null,
    deposit_lock_pence: 0,
    collateral_value_pence: 0,
    collateral_status: "none",
    status: "active",
    outstanding_principal_pence: 66_667,
    accrued_interest_pence: 100,
    owed_pence: 66_767,
    principal_repaid_pence: 33_333,
    interest_paid_pence: 274,
    collateral_credit_pence: 0,
    extinguished_pence: 0,
    written_off_pence: 0,
    foreclosure_surplus_paid_pence: 0,
    foreclosure_surplus_owed_pence: 0,
    offer_expires_at: "2026-11-02T12:00:00Z",
    disbursed_at: "2026-11-02T12:00:00Z",
    matures_at: "2026-11-04T12:00:00Z",
    defaulted_at: null,
    closed_at: null,
    interest_accrued_through: observedAt,
    request_id: null,
    quote_id: null,
    created_at: "2026-11-02T10:00:00Z",
    updated_at: observedAt,
    lender_name: "Harbour Bank",
    observed_at: observedAt,
    remaining_installments: 2,
    remaining_installments_derived: true,
    installment_note: "Derived from the recorded schedule.",
    ...overrides,
  };
}

function account(overrides: Partial<LoanAccount> = {}): LoanAccount {
  return {
    account_id: "account-1",
    label: "Yukine",
    company_id: "borrower-1",
    observed_at: observedAt,
    history_refreshed_at: observedAt,
    history_attempted_at: observedAt,
    history_complete: true,
    history_error: "",
    recording_error: "",
    loans_stale: false,
    loans_error: "",
    active_loans: [],
    closed_loans: [],
    totals: {
      owed_pence: 0,
      principal_repaid_pence: 0,
      interest_paid_pence: 0,
      ledger_principal_repaid_pence: 0,
      ledger_interest_paid_pence: 0,
      principal_difference_pence: 0,
      interest_difference_pence: 0,
      repaid_count: 0,
    },
    ...overrides,
  };
}

function totals(overrides: Partial<LoanAccount["totals"]>) {
  return { ...account().totals, ...overrides };
}

describe("recorded borrower loan summary", () => {
  it("aggregates only supplied account scope and keeps row lifetime totals authoritative", () => {
    const first = account({
      active_loans: [loan()],
      totals: totals({
        owed_pence: 66_767,
        principal_repaid_pence: 33_333,
        interest_paid_pence: 274,
        ledger_interest_paid_pence: 100,
        principal_difference_pence: 33_333,
        interest_difference_pence: 174,
        repaid_count: 1,
      }),
    });
    const second = account({
      account_id: "account-2",
      company_id: "borrower-2",
      active_loans: [loan({ id: "loan-2", remaining_installments: 3 })],
      totals: totals({
        owed_pence: 1_000,
        principal_repaid_pence: 2_000,
        interest_paid_pence: 300,
        ledger_principal_repaid_pence: 9_000,
        ledger_interest_paid_pence: 1_000,
        principal_difference_pence: -7_000,
        interest_difference_pence: -700,
        repaid_count: 2,
      }),
    });
    const all = summarizeLoans([first, second]);
    expect(all.owedPence).toBe(67_767);
    expect(all.principalRepaidPence).toBe(35_333);
    expect(all.interestPaidPence).toBe(574);
    expect(all.repaidCount).toBe(3);
    expect(all.remainingInstallments).toBe(5);
    expect(summarizeLoans([second]).principalRepaidPence).toBe(2_000);
    expect(summarizeLoans([second]).active[0].account.account_id).toBe(
      "account-2",
    );
  });

  it("keeps recovered and written-off debt separate from fully repaid loans", () => {
    const result = summarizeLoans([
      account({
        closed_loans: [
          loan({ id: "paid", status: "repaid", owed_pence: 0 }),
          loan({
            id: "recovered",
            status: "recovered",
            collateral_credit_pence: 80_000,
          }),
          loan({
            id: "lost",
            status: "written_off",
            written_off_pence: 100_000,
          }),
        ],
        totals: totals({ principal_repaid_pence: 5_000, repaid_count: 1 }),
      }),
    ]);
    expect(result.repaid.map((row) => row.loan.id)).toEqual(["paid"]);
    expect(result.otherClosed.map((row) => row.loan.status)).toEqual([
      "recovered",
      "written_off",
    ]);
    expect(result.principalRepaidPence).toBe(5_000);
    expect(result.repaidCount).toBe(1);
  });

  it("requires a zero balance for the fully repaid table and count", () => {
    const result = summarizeLoans([
      account({
        closed_loans: [
          loan({ id: "paid", status: "repaid", owed_pence: 0 }),
          loan({ id: "inconsistent", status: "repaid", owed_pence: 1 }),
        ],
        totals: totals({ repaid_count: 1 }),
      }),
    ]);
    expect(result.repaid.map((row) => row.loan.id)).toEqual(["paid"]);
    expect(result.otherClosed.map((row) => row.loan.id)).toEqual([
      "inconsistent",
    ]);
    expect(result.repaidCount).toBe(result.repaid.length);
  });

  it("does not fabricate zeros for an account without loan observations", () => {
    const result = summarizeLoans([
      account({
        observed_at: null,
        history_refreshed_at: null,
        history_attempted_at: null,
        history_complete: false,
      }),
    ]);
    expect(result.owedPence).toBeUndefined();
    expect(result.principalRepaidPence).toBeUndefined();
    expect(result.interestPaidPence).toBeUndefined();
    expect(result.repaidCount).toBeUndefined();
    expect(result.remainingInstallments).toBeUndefined();
    expect(result.unobservedAccounts).toBe(1);
    expect(result.nextPayment).toBeUndefined();
    expect(summarizeLoans([]).owedPence).toBeUndefined();
  });

  it("shows an observed empty loan book as zero rather than unavailable", () => {
    const result = summarizeLoans([account()]);
    expect(result.owedPence).toBe(0);
    expect(result.principalRepaidPence).toBe(0);
    expect(result.interestPaidPence).toBe(0);
    expect(result.repaidCount).toBe(0);
    expect(result.remainingInstallments).toBe(0);
  });

  it("retains stored payment totals through incomplete or failed history refreshes", () => {
    const result = summarizeLoans([
      account({
        history_complete: false,
        history_error: "Full capture interrupted",
        loans_stale: true,
        totals: totals({
          principal_repaid_pence: 8_000,
          interest_paid_pence: 900,
        }),
      }),
    ]);
    expect(result.principalRepaidPence).toBe(8_000);
    expect(result.interestPaidPence).toBe(900);
  });

  it("keeps known installments but makes the total unknown if any schedule is unknown", () => {
    const result = summarizeLoans([
      account({
        active_loans: [
          loan({ remaining_installments: 4 }),
          loan({
            id: "defaulted",
            status: "defaulted",
            remaining_installments: null,
          }),
        ],
      }),
    ]);
    expect(result.knownInstallments).toBe(4);
    expect(result.unknownInstallmentLoans).toBe(1);
    expect(result.remainingInstallments).toBeUndefined();
    const missingAccount = account({
      account_id: "missing",
      observed_at: null,
    });
    expect(
      summarizeLoans([account({ active_loans: [loan()] }), missingAccount])
        .remainingInstallments,
    ).toBeUndefined();
  });

  it("sums simultaneous earliest payments by timestamp without replacing missing amounts", () => {
    const result = summarizeLoans([
      account({
        active_loans: [
          loan({
            id: "later",
            next_payment_due_at: "2026-11-04T12:00:00Z",
            next_payment_pence: 900,
          }),
          loan({ id: "first", next_payment_pence: 200 }),
        ],
      }),
      account({
        account_id: "account-2",
        active_loans: [
          loan({
            id: "same",
            next_payment_due_at: "2026-11-03T13:00:00+01:00",
            next_payment_pence: 300,
          }),
        ],
      }),
    ]);
    expect(result.nextPayment?.amountPence).toBe(500);
    expect(result.nextPayment?.rows.map((row) => row.loan.id)).toEqual([
      "first",
      "same",
    ]);
    const unknown = summarizeLoans([
      account({
        active_loans: [
          loan({ next_payment_pence: null }),
          loan({ id: "same", next_payment_pence: 300 }),
        ],
      }),
    ]);
    expect(unknown.nextPayment?.amountPence).toBeUndefined();
    expect(unknown.nextPayment?.rows).toHaveLength(2);
  });

  it("does not infer missing due dates from maturity or skip an earliest unknown amount", () => {
    const result = summarizeLoans([
      account({
        active_loans: [
          loan({ id: "undated", next_payment_due_at: null }),
          loan({ id: "invalid", next_payment_due_at: "not-a-date" }),
          loan({ id: "first", next_payment_pence: null }),
          loan({
            id: "later",
            next_payment_due_at: "2026-11-04T12:00:00Z",
            next_payment_pence: 900,
          }),
          loan({
            id: "zero-debt",
            owed_pence: 0,
            next_payment_due_at: "2026-11-02T12:00:00Z",
          }),
        ],
      }),
    ]);
    expect(result.unknownNextPaymentLoans).toBe(2);
    expect(result.nextPayment?.rows[0].loan.id).toBe("first");
    expect(result.nextPayment?.amountPence).toBeUndefined();
  });

  it("uses the oldest actual loan observation, not the report read time", () => {
    const result = summarizeLoans([
      account({ observed_at: "2026-11-03T11:00:00Z" }),
      account({ account_id: "second", observed_at: observedAt }),
      account({ account_id: "missing", observed_at: null }),
    ]);
    expect(result.oldestObservation).toBe(observedAt);
    expect(result.unobservedAccounts).toBe(1);
  });

  it("does not treat defaulted debt as a scheduled or missing next payment", () => {
    const result = summarizeLoans([
      account({
        active_loans: [
          loan({
            id: "defaulted-old-date",
            status: "defaulted",
            next_payment_due_at: "2026-11-02T01:00:00Z",
            remaining_installments: null,
            arrears_pence: 1_000,
          }),
          loan({
            id: "defaulted-no-date",
            status: "defaulted",
            next_payment_due_at: null,
            remaining_installments: null,
          }),
          loan({ id: "scheduled" }),
        ],
      }),
    ]);
    expect(result.defaultedLoans).toBe(2);
    expect(result.nextPayment?.rows.map((row) => row.loan.id)).toEqual([
      "scheduled",
    ]);
    expect(result.unknownNextPaymentLoans).toBe(0);
    expect(result.arrearsPence).toBe(0);
    expect(result.remainingInstallments).toBeUndefined();
  });

  it("keeps arrears grace amounts and deadlines separate from the regular schedule", () => {
    const result = summarizeLoans([
      account({
        active_loans: [
          loan({
            id: "first",
            arrears_pence: 1_000,
            arrears_due_by: "2026-11-03T11:00:00Z",
          }),
          loan({
            id: "same",
            arrears_pence: 2_000,
            arrears_due_by: "2026-11-03T12:00:00+01:00",
          }),
          loan({
            id: "later",
            arrears_pence: 3_000,
            arrears_due_by: "2026-11-03T11:01:00Z",
          }),
          loan({ id: "unknown", arrears_pence: 4_000, arrears_due_by: null }),
        ],
      }),
    ]);
    expect(result.arrearsPence).toBe(10_000);
    expect(result.arrearsDeadline?.dueAt).toBe("2026-11-03T11:00:00Z");
    expect(result.arrearsDeadline?.amountPence).toBe(3_000);
    expect(result.arrearsDeadline?.rows.map((row) => row.loan.id)).toEqual([
      "first",
      "same",
    ]);
    expect(result.unknownArrearsDeadlineLoans).toBe(1);
    expect(result.nextPayment?.dueAt).toBe("2026-11-03T12:00:00Z");
  });

  it("keeps a reported grace payment without claiming an installment count", () => {
    const result = summarizeLoans([
      account({
        active_loans: [
          loan({
            arrears_pence: 1_000,
            arrears_due_by: "2026-11-03T12:00:00Z",
            next_payment_due_at: "2026-11-03T12:00:00Z",
            next_payment_pence: 1_000,
            remaining_installments: null,
            remaining_installments_derived: false,
          }),
        ],
      }),
    ]);
    expect(result.nextPayment?.dueAt).toBe(result.arrearsDeadline?.dueAt);
    expect(result.nextPayment?.amountPence).toBe(1_000);
    expect(result.remainingInstallments).toBeUndefined();
    expect(result.unknownInstallmentLoans).toBe(1);
  });
});

describe("loan presentation boundaries", () => {
  it.each([null, -1, 1.5, Number.NaN, Number.POSITIVE_INFINITY])(
    "leaves invalid or missing installment count %s unavailable for sorting",
    (value) => {
      expect(
        loanInstallments(loan({ remaining_installments: value })),
      ).toBeUndefined();
    },
  );

  it("preserves legitimate zero and integer schedule counts", () => {
    expect(loanInstallments(loan({ remaining_installments: 0 }))).toBe(0);
    expect(loanInstallments(loan({ remaining_installments: 18 }))).toBe(18);
  });

  it("provides numeric timestamp sorts and leaves missing dates undefined", () => {
    expect(loanTimestamp(null)).toBeUndefined();
    expect(loanTimestamp("invalid")).toBeUndefined();
    expect(loanTimestamp("2026-11-03T11:00:00+01:00")).toBe(
      Date.parse(observedAt),
    );
  });

  it("marks loan observations stale at two minutes without treating unknown times as fresh", () => {
    const observed = Date.parse(observedAt);
    expect(loanObservationStale(observedAt, observed + 119_999)).toBe(false);
    expect(loanObservationStale(observedAt, observed + 120_000)).toBe(true);
    expect(loanObservationStale(observedAt, observed + 300_000)).toBe(true);
    expect(loanObservationStale(null, observed + 300_000)).toBe(false);
    expect(loanObservationStale("invalid", observed + 300_000)).toBe(false);
    expect(loanObservationStale(observedAt, 0)).toBe(false);
    expect(loanObservationStale(observedAt, Number.NaN)).toBe(false);
  });

  it("uses the random UUID suffix rather than colliding time prefixes", () => {
    expect(shortLoanID("01a19be9-8d23-732f-826c-fb3f56dd2d31")).toBe(
      "56dd2d31",
    );
    expect(shortLoanID("01a19be9-8d23-732f-826c-fb3f56dd2d32")).toBe(
      "56dd2d32",
    );
    expect(shortLoanID("short")).toBe("short");
    expect(shortLoanID("")).toBe("Not recorded");
  });

  it.each([
    ["2026-11-03T10:00:00Z", "Due now"],
    ["2026-11-03T10:00:01Z", "In 1m"],
    ["2026-11-03T12:30:00Z", "In 2h 30m"],
    ["2026-11-05T12:00:00Z", "In 2d 2h"],
    ["2026-11-03T09:59:00Z", "Due time passed 1m ago"],
    ["2026-11-02T08:00:00Z", "Due time passed 1d 2h ago"],
  ])("formats a real UTC payment %s as %s", (value, expected) => {
    expect(paymentTiming(value, Date.parse(observedAt))).toBe(expected);
  });

  it("does not invent relative timing without a valid timestamp or browser clock", () => {
    expect(paymentTiming(null, Date.parse(observedAt))).toBe("Not recorded");
    expect(paymentTiming("invalid", Date.parse(observedAt))).toBe(
      "Not recorded",
    );
    expect(paymentTiming(observedAt, 0)).toBe("Time comparison unavailable");
  });
});
