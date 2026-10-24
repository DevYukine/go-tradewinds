-- +goose Up
-- These are permanent economic/recovery records, not seven-day diagnostics.
CREATE TABLE underwriting_policies (
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    policy_id text NOT NULL,
    terms_hash text NOT NULL,
    status text NOT NULL,
    payload jsonb NOT NULL,
    source_updated_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    terms_changed boolean NOT NULL DEFAULT false,
    PRIMARY KEY (account_id, policy_id),
    CHECK (length(policy_id) BETWEEN 1 AND 255),
    CHECK (length(terms_hash) BETWEEN 1 AND 255),
    CHECK (length(status) BETWEEN 1 AND 255),
    CHECK (jsonb_typeof(payload) = 'object')
);

CREATE TABLE underwriting_budgets (
    account_id text PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    initial_pence bigint NOT NULL CHECK (initial_pence >= 0),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE underwriting_positions (
    id text NOT NULL,
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    action_id text NOT NULL REFERENCES actions(id),
    policy_id text NOT NULL,
    company_id text NOT NULL,
    slice_bps bigint NOT NULL CHECK (slice_bps = 500),
    collateral_pence bigint NOT NULL CHECK (collateral_pence BETWEEN 1 AND 1000000000000),
    premium_pence bigint NOT NULL CHECK (premium_pence BETWEEN 0 AND 1000000000000),
    frozen_policy jsonb NOT NULL CHECK (jsonb_typeof(frozen_policy) = 'object'),
    funding_evidence jsonb CHECK (jsonb_typeof(funding_evidence) = 'object'),
    accepted_at timestamptz NOT NULL,
    status text NOT NULL DEFAULT 'active',
    earned_premium_pence bigint CHECK (earned_premium_pence BETWEEN 0 AND 1000000000000),
    claims_pence bigint CHECK (claims_pence BETWEEN 0 AND 1000000000000),
    returned_collateral_pence bigint CHECK (returned_collateral_pence BETWEEN 0 AND 1000000000000),
    fees_pence bigint CHECK (fees_pence BETWEEN 0 AND 1000000000000),
    net_pence bigint,
    closed_at timestamptz,
    closure_evidence jsonb,
    PRIMARY KEY (account_id, id),
    UNIQUE (account_id, action_id),
    CHECK (length(id) BETWEEN 1 AND 255),
    CHECK (length(policy_id) BETWEEN 1 AND 255),
    CHECK (length(company_id) BETWEEN 1 AND 255),
    CHECK (length(status) BETWEEN 1 AND 255),
    CHECK (
        (closed_at IS NULL AND status NOT IN ('settled_safe', 'claimed_loss')
            AND earned_premium_pence IS NULL AND claims_pence IS NULL
            AND returned_collateral_pence IS NULL AND fees_pence IS NULL
            AND net_pence IS NULL AND closure_evidence IS NULL)
        OR (closed_at IS NOT NULL AND status IN ('settled_safe', 'claimed_loss')
            AND earned_premium_pence IS NOT NULL AND claims_pence IS NOT NULL
            AND returned_collateral_pence IS NOT NULL AND fees_pence IS NOT NULL
            AND net_pence IS NOT NULL AND closure_evidence IS NOT NULL
            AND jsonb_typeof(closure_evidence) = 'object')
    ),
    CHECK (closed_at >= accepted_at),
    CHECK (earned_premium_pence <= premium_pence),
    CHECK (claims_pence::numeric + returned_collateral_pence = collateral_pence),
    CHECK (net_pence::numeric = earned_premium_pence::numeric - claims_pence - fees_pence),
    CHECK (status <> 'settled_safe' OR claims_pence = 0),
    CHECK (status <> 'claimed_loss' OR claims_pence = collateral_pence)
);
CREATE INDEX underwriting_positions_account_accepted_idx ON underwriting_positions (account_id, accepted_at, id);

-- +goose StatementBegin
CREATE FUNCTION guard_underwriting_policy_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.account_id, NEW.policy_id, NEW.terms_hash)
       IS DISTINCT FROM ROW(OLD.account_id, OLD.policy_id, OLD.terms_hash) THEN
        RAISE EXCEPTION 'first underwriting policy terms are immutable';
    END IF;
    IF OLD.terms_changed AND NOT NEW.terms_changed THEN
        RAISE EXCEPTION 'underwriting policy censorship is irreversible';
    END IF;
    IF NEW.source_updated_at < OLD.source_updated_at OR NEW.observed_at < OLD.observed_at THEN
        RAISE EXCEPTION 'underwriting policy evidence cannot move backwards';
    END IF;
    IF OLD.status IN ('settled_safe', 'claimed_loss', 'cancelled')
       AND NEW.status NOT IN ('settled_safe', 'claimed_loss', 'cancelled') THEN
        RAISE EXCEPTION 'terminal underwriting policy cannot reopen';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER underwriting_policy_evidence_immutable BEFORE UPDATE ON underwriting_policies
    FOR EACH ROW EXECUTE FUNCTION guard_underwriting_policy_evidence();

-- +goose StatementBegin
CREATE FUNCTION guard_underwriting_position_immutability() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.id, NEW.account_id, NEW.action_id, NEW.policy_id, NEW.company_id,
           NEW.slice_bps, NEW.collateral_pence, NEW.premium_pence, NEW.frozen_policy, NEW.accepted_at)
       IS DISTINCT FROM
       ROW(OLD.id, OLD.account_id, OLD.action_id, OLD.policy_id, OLD.company_id,
           OLD.slice_bps, OLD.collateral_pence, OLD.premium_pence, OLD.frozen_policy, OLD.accepted_at) THEN
        RAISE EXCEPTION 'underwriting acceptance is immutable';
    END IF;
    IF OLD.funding_evidence IS NOT NULL AND NEW.funding_evidence IS DISTINCT FROM OLD.funding_evidence THEN
        RAISE EXCEPTION 'underwriting funding proof is immutable';
    END IF;
    IF OLD.closed_at IS NOT NULL AND
       ROW(NEW.status, NEW.earned_premium_pence, NEW.claims_pence, NEW.returned_collateral_pence,
           NEW.fees_pence, NEW.net_pence, NEW.closed_at, NEW.closure_evidence)
       IS DISTINCT FROM
       ROW(OLD.status, OLD.earned_premium_pence, OLD.claims_pence, OLD.returned_collateral_pence,
           OLD.fees_pence, OLD.net_pence, OLD.closed_at, OLD.closure_evidence) THEN
        RAISE EXCEPTION 'underwriting closure is immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER underwriting_position_immutable BEFORE UPDATE ON underwriting_positions
    FOR EACH ROW EXECUTE FUNCTION guard_underwriting_position_immutability();

-- +goose StatementBegin
CREATE FUNCTION guard_underwriting_budget_immutability() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'initial underwriting budget is immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER underwriting_budget_immutable BEFORE UPDATE ON underwriting_budgets
    FOR EACH ROW EXECUTE FUNCTION guard_underwriting_budget_immutability();

-- +goose Down
DROP TRIGGER underwriting_budget_immutable ON underwriting_budgets;
DROP FUNCTION guard_underwriting_budget_immutability();
DROP TRIGGER underwriting_position_immutable ON underwriting_positions;
DROP FUNCTION guard_underwriting_position_immutability();
DROP TRIGGER underwriting_policy_evidence_immutable ON underwriting_policies;
DROP FUNCTION guard_underwriting_policy_evidence();
DROP TABLE underwriting_positions;
DROP TABLE underwriting_budgets;
DROP TABLE underwriting_policies;
