export interface JournalEntry {
  id: string;
  account_id: string;
  at: string;
  kind: string;
  ship_id?: string;
  summary: string;
  amount_pence?: number;
  amount_kind?: "realized_net" | "charge" | "cash_in" | "cash_out";
  costs_known?: boolean;
  origin?: string;
  destination?: string;
  commodity?: string;
  quantity_tons?: number;
}

export function journalCashAmount(entry: JournalEntry): number | undefined {
  if (
    (entry.kind === "settlement" && entry.costs_known !== true) ||
    (entry.amount_kind === "realized_net" && entry.costs_known === false)
  ) {
    return undefined;
  }
  return entry.amount_pence;
}
