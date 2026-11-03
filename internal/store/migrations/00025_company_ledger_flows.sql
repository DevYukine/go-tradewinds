-- +goose Up
CREATE TABLE company_ledger_flows (
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    ledger_id text NOT NULL,
    reason text NOT NULL,
    amount_pence bigint NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (account_id, ledger_id)
);
CREATE INDEX company_ledger_flows_account_created_idx ON company_ledger_flows (account_id, created_at);

-- +goose Down
DROP TABLE company_ledger_flows;
