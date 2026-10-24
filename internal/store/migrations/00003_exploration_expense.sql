-- +goose Up
ALTER TABLE settlements ADD COLUMN exploration_expense_pence bigint;
ALTER TABLE settlements ADD CONSTRAINT settlements_exploration_expense_check
    CHECK (exploration_expense_pence IS NULL OR exploration_expense_pence >= 0);

-- +goose Down
ALTER TABLE settlements DROP CONSTRAINT settlements_exploration_expense_check;
ALTER TABLE settlements DROP COLUMN exploration_expense_pence;
