-- +goose Up
CREATE TABLE loan_history (
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    loan_id text NOT NULL,
    data jsonb NOT NULL CHECK (jsonb_typeof(data) = 'object'),
    status text NOT NULL,
    closed_at timestamptz,
    observed_at timestamptz NOT NULL,
    source_updated_at timestamptz,
    PRIMARY KEY (account_id, loan_id)
);

-- +goose Down
DROP TABLE loan_history;
