-- +goose Up
ALTER TABLE actions ADD COLUMN retry_after timestamptz;
ALTER TABLE actions ADD CONSTRAINT actions_retry_after_outcome_check
    CHECK (retry_after IS NULL OR (state = 'complete' AND status = 429));

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION guard_action_immutability() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.id, NEW.account_id, NEW.company_id, NEW.method, NEW.path,
           NEW.body_cipher, NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.id, OLD.account_id, OLD.company_id, OLD.method, OLD.path,
           OLD.body_cipher, OLD.created_at) THEN
        RAISE EXCEPTION 'action request is immutable';
    END IF;
    IF OLD.state <> 'pending' AND
       ROW(NEW.state, NEW.status, NEW.response_cipher, NEW.retry_after)
       IS DISTINCT FROM ROW(OLD.state, OLD.status, OLD.response_cipher, OLD.retry_after) THEN
        RAISE EXCEPTION 'action result is immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION guard_action_immutability() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.id, NEW.account_id, NEW.company_id, NEW.method, NEW.path,
           NEW.body_cipher, NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.id, OLD.account_id, OLD.company_id, OLD.method, OLD.path,
           OLD.body_cipher, OLD.created_at) THEN
        RAISE EXCEPTION 'action request is immutable';
    END IF;
    IF OLD.state <> 'pending' AND
       ROW(NEW.state, NEW.status, NEW.response_cipher)
       IS DISTINCT FROM ROW(OLD.state, OLD.status, OLD.response_cipher) THEN
        RAISE EXCEPTION 'action result is immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

ALTER TABLE actions DROP CONSTRAINT actions_retry_after_outcome_check;
ALTER TABLE actions DROP COLUMN retry_after;
