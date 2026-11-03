import type { LoanAccount, LoanEntry } from "./types";

export interface LoanRow {
  account: LoanAccount;
  loan: LoanEntry;
}

export function loanTimestamp(value: string | null | undefined) {
  const timestamp = Date.parse(value ?? "");
  return Number.isFinite(timestamp) ? timestamp : undefined;
}

export function loanObservationStale(
  value: string | null | undefined,
  now: number,
) {
  const timestamp = loanTimestamp(value);
  return (
    timestamp !== undefined &&
    Number.isFinite(now) &&
    now > 0 &&
    now - timestamp >= 120_000
  );
}

export function loanInstallments(loan: LoanEntry) {
  const value = loan.remaining_installments;
  return value !== null && Number.isSafeInteger(value) && value >= 0
    ? value
    : undefined;
}

export function shortLoanID(value: string) {
  return value ? value.slice(-8) : "Not recorded";
}

export function paymentTiming(value: string | null | undefined, now: number) {
  const timestamp = loanTimestamp(value);
  if (timestamp === undefined) {
    return "Not recorded";
  }
  if (!now || !Number.isFinite(now)) {
    return "Time comparison unavailable";
  }
  const difference = timestamp - now;
  if (difference === 0) {
    return "Due now";
  }
  const minutes = Math.ceil(Math.abs(difference) / 60_000);
  let duration: string;
  if (minutes < 60) {
    duration = `${minutes}m`;
  } else if (minutes < 1_440) {
    duration = `${Math.floor(minutes / 60)}h ${minutes % 60}m`;
  } else {
    duration = `${Math.floor(minutes / 1_440)}d ${Math.floor((minutes % 1_440) / 60)}h`;
  }
  return difference > 0 ? `In ${duration}` : `Due time passed ${duration} ago`;
}

export function summarizeLoans(accounts: LoanAccount[]) {
  const active: LoanRow[] = [];
  const repaid: LoanRow[] = [];
  const otherClosed: LoanRow[] = [];
  let owedPence = 0;
  let principalRepaidPence = 0;
  let interestPaidPence = 0;
  let repaidCount = 0;
  let knownInstallments = 0;
  let unknownInstallmentLoans = 0;
  let unknownNextPaymentLoans = 0;
  let unobservedAccounts = 0;
  let oldestObservation: string | undefined;
  let defaultedLoans = 0;
  let arrearsPence = 0;
  let unknownArrearsDeadlineLoans = 0;
  let arrearsDeadline:
    { dueAt: string; amountPence: number; rows: LoanRow[] } | undefined;
  let arrearsTimestamp: number | undefined;
  let hasRecords = false;
  let nextPayment:
    | { dueAt: string; amountPence: number | undefined; rows: LoanRow[] }
    | undefined;
  let nextTimestamp: number | undefined;

  for (const account of accounts) {
    owedPence += account.totals.owed_pence;
    principalRepaidPence += account.totals.principal_repaid_pence;
    interestPaidPence += account.totals.interest_paid_pence;
    repaidCount += account.totals.repaid_count;
    const observed = loanTimestamp(account.observed_at);
    if (observed === undefined) {
      unobservedAccounts += 1;
    } else if (
      oldestObservation === undefined ||
      observed < loanTimestamp(oldestObservation)!
    ) {
      oldestObservation = account.observed_at!;
    }
    hasRecords ||=
      observed !== undefined ||
      account.history_complete ||
      account.active_loans.length > 0 ||
      account.closed_loans.length > 0;

    for (const loan of account.closed_loans) {
      const row = { account, loan };
      if (loan.status === "repaid" && loan.owed_pence === 0) {
        repaid.push(row);
      } else {
        otherClosed.push(row);
      }
    }
    for (const loan of account.active_loans) {
      const row = { account, loan };
      active.push(row);
      const installments = loanInstallments(loan);
      if (installments === undefined) {
        unknownInstallmentLoans += 1;
      } else {
        knownInstallments += installments;
      }
      if (loan.status === "defaulted") {
        defaultedLoans += 1;
        continue;
      }
      if (loan.owed_pence <= 0) {
        continue;
      }
      if (loan.arrears_pence > 0) {
        arrearsPence += loan.arrears_pence;
        const deadline = loanTimestamp(loan.arrears_due_by);
        if (deadline === undefined) {
          unknownArrearsDeadlineLoans += 1;
        } else if (
          arrearsTimestamp === undefined ||
          deadline < arrearsTimestamp
        ) {
          arrearsTimestamp = deadline;
          arrearsDeadline = {
            dueAt: loan.arrears_due_by!,
            amountPence: loan.arrears_pence,
            rows: [row],
          };
        } else if (deadline === arrearsTimestamp && arrearsDeadline) {
          arrearsDeadline.amountPence += loan.arrears_pence;
          arrearsDeadline.rows.push(row);
        }
      }
      const due = loanTimestamp(loan.next_payment_due_at);
      if (due === undefined) {
        unknownNextPaymentLoans += 1;
        continue;
      }
      const amount = loan.next_payment_pence;
      const knownAmount =
        amount !== null && Number.isFinite(amount) && amount >= 0
          ? amount
          : undefined;
      if (nextTimestamp === undefined || due < nextTimestamp) {
        nextTimestamp = due;
        nextPayment = {
          dueAt: loan.next_payment_due_at!,
          amountPence: knownAmount,
          rows: [row],
        };
      } else if (due === nextTimestamp && nextPayment) {
        nextPayment.rows.push(row);
        nextPayment.amountPence =
          nextPayment.amountPence !== undefined && knownAmount !== undefined
            ? nextPayment.amountPence + knownAmount
            : undefined;
      }
    }
  }

  return {
    active,
    repaid,
    otherClosed,
    owedPence: accounts.length && !unobservedAccounts ? owedPence : undefined,
    principalRepaidPence: hasRecords ? principalRepaidPence : undefined,
    interestPaidPence: hasRecords ? interestPaidPence : undefined,
    repaidCount: hasRecords ? repaidCount : undefined,
    remainingInstallments:
      accounts.length && !unobservedAccounts && !unknownInstallmentLoans
        ? knownInstallments
        : undefined,
    knownInstallments,
    unknownInstallmentLoans,
    unknownNextPaymentLoans,
    unobservedAccounts,
    oldestObservation,
    defaultedLoans,
    arrearsPence,
    arrearsDeadline,
    unknownArrearsDeadlineLoans,
    nextPayment,
  };
}
