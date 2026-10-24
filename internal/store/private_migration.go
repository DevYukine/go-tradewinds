package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlog "gorm.io/gorm/logger"
)

// Goose runs this closure and its version-table write in the same transaction.
// There is deliberately no startup repair or plaintext-compatible reader.
func (s *Store) migratePrivateEvidence(ctx context.Context, sqlTx *sql.Tx) error {
	tx, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlTx}), &gorm.Config{
		DisableAutomaticPing:   true,
		SkipDefaultTransaction: true,
		Logger:                 gormlog.Default.LogMode(gormlog.Silent),
	})
	if err != nil {
		return err
	}
	tx = tx.WithContext(ctx)
	// Block old-schema readers/writers for the entire conversion. Executor
	// session locks below are a separate fence from Goose's migration lock.
	if err := tx.Exec(`LOCK TABLE accounts, account_secrets, missions, settlements,
		underwriting_policies, underwriting_positions, underwriting_budgets, decisions
		IN ACCESS EXCLUSIVE MODE`).Error; err != nil {
		return err
	}
	credentialsByAccount, err := s.authenticatedSecrets(tx)
	if err != nil {
		return fmt.Errorf("authenticate credentials before private evidence migration: %w", err)
	}
	var accounts []Account
	if err := tx.Order("id").Find(&accounts).Error; err != nil {
		return err
	}
	for _, account := range accounts {
		identities := []string{"tradewinds:runner:account:" + account.ID}
		if account.CompanyID != "" {
			identities = append(identities, "tradewinds:runner:company:"+account.CompanyID)
		}
		for _, identity := range identities {
			digest := sha256.Sum256([]byte(identity))
			key := int64(binary.BigEndian.Uint64(digest[:8]))
			var acquired bool
			if err := tx.Raw("SELECT pg_try_advisory_xact_lock(?)", key).Scan(&acquired).Error; err != nil {
				return err
			}
			if !acquired {
				return fmt.Errorf("private evidence migration requires stopped account and company executors: %w", ErrAccountLocked)
			}
		}
	}
	if err := tx.Exec(`
		ALTER TABLE missions ADD COLUMN ship_lookup text, ADD COLUMN metadata_cipher bytea, ADD COLUMN data_cipher bytea;
		ALTER TABLE settlements ADD COLUMN lane_lookup text, ADD COLUMN context_cipher bytea;
		ALTER TABLE underwriting_policies ADD COLUMN policy_lookup text, ADD COLUMN identity_cipher bytea, ADD COLUMN payload_cipher bytea;
		ALTER TABLE underwriting_positions ADD COLUMN position_lookup text, ADD COLUMN identity_cipher bytea, ADD COLUMN frozen_cipher bytea;
		DROP TRIGGER underwriting_policy_evidence_immutable ON underwriting_policies;
		DROP TRIGGER underwriting_position_immutable ON underwriting_positions;
	`).Error; err != nil {
		return err
	}
	if err := s.migrateMissionRows(tx); err != nil {
		return err
	}
	if err := s.migrateSettlementRows(tx); err != nil {
		return err
	}
	if err := s.migrateUnderwritingRows(tx); err != nil {
		return err
	}
	// Legacy diagnostic writes already scrubbed kind, reason and details.
	// Ship identity was the uncovered column; deduplicate it before backfill
	// so a long diagnostic history does not become one UPDATE per observation.
	var identities []struct {
		AccountID string
		ShipID    string
	}
	if err := tx.Table("decisions").Select("DISTINCT account_id, ship_id").Find(&identities).Error; err != nil {
		return err
	}
	for _, identity := range identities {
		secrets, available := credentialsByAccount[identity.AccountID]
		cleanShipID := displayText(identity.ShipID, displayCredentials{secrets: secrets, available: available})
		if cleanShipID == identity.ShipID {
			continue
		}
		if err := tx.Table("decisions").Where("account_id = ? AND ship_id = ?", identity.AccountID, identity.ShipID).
			Update("ship_id", cleanShipID).Error; err != nil {
			return err
		}
	}
	if err := tx.Exec(privateEvidenceConstraints).Error; err != nil {
		return err
	}
	return tx.Exec("INSERT INTO private_evidence_key (singleton, verifier) VALUES (true, ?)", s.keyVerifier()).Error
}

func (s *Store) migrateMissionRows(tx *gorm.DB) error {
	var stored []struct {
		Mission  `gorm:"embedded"`
		DataText string
	}
	if err := tx.Table("missions").Select("account_id, ship_id, phase, destination, commodity, contract_id, quantity, price_pence, generation, data::text AS data_text, updated_at").Find(&stored).Error; err != nil {
		return err
	}
	for _, legacy := range stored {
		value := legacy.Mission
		value.Data = []byte(legacy.DataText)
		if err := s.migrateMissionData(tx, &value); err != nil {
			return err
		}
		row, err := s.encodeMission(value)
		if err != nil {
			return err
		}
		if err := tx.Table("missions").Where("account_id = ? AND ship_id = ?", value.AccountID, value.ShipID).Updates(map[string]any{
			"ship_lookup": row.ShipLookup, "metadata_cipher": row.MetadataCipher, "data_cipher": row.DataCipher,
		}).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) migrateSettlementRows(tx *gorm.DB) error {
	var values []Settlement
	if err := tx.Table("settlements").Find(&values).Error; err != nil {
		return err
	}
	for _, value := range values {
		row, err := s.encodeSettlement(value)
		if err != nil {
			return err
		}
		if err := tx.Table("settlements").Where("id = ?", value.ID).Updates(map[string]any{
			"lane_lookup": row.LaneLookup, "context_cipher": row.ContextCipher, "kind": row.Kind,
		}).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) migrateUnderwritingRows(tx *gorm.DB) error {
	var policies []struct {
		UnderwritingPolicyRecord `gorm:"embedded"`
		PayloadText              string
	}
	if err := tx.Table("underwriting_policies").Select("account_id, policy_id, terms_hash, status, payload::text AS payload_text, source_updated_at, observed_at, terms_changed").Find(&policies).Error; err != nil {
		return err
	}
	for _, legacy := range policies {
		value := legacy.UnderwritingPolicyRecord
		value.Payload = []byte(legacy.PayloadText)
		row, err := s.encodeUnderwritingPolicy(value)
		if err != nil {
			return err
		}
		if err := tx.Table("underwriting_policies").Where("account_id = ? AND policy_id = ?", value.AccountID, value.PolicyID).Updates(map[string]any{
			"policy_lookup": row.PolicyLookup, "identity_cipher": row.IdentityCipher, "payload_cipher": row.PayloadCipher, "status": row.Status,
		}).Error; err != nil {
			return err
		}
	}
	var positions []struct {
		UnderwritingPosition `gorm:"embedded"`
		FrozenPolicyText     string
		FundingEvidenceText  *string
	}
	if err := tx.Table("underwriting_positions").Select(`id, account_id, action_id, policy_id, company_id,
		slice_bps, collateral_pence, premium_pence, accepted_at, status, earned_premium_pence,
		claims_pence, returned_collateral_pence, fees_pence, net_pence, closed_at,
		frozen_policy::text AS frozen_policy_text, funding_evidence::text AS funding_evidence_text`).Find(&positions).Error; err != nil {
		return err
	}
	for _, legacy := range positions {
		value := legacy.UnderwritingPosition
		value.FrozenPolicy = []byte(legacy.FrozenPolicyText)
		if legacy.FundingEvidenceText != nil {
			value.FundingEvidence = []byte(*legacy.FundingEvidenceText)
		}
		row, err := s.encodeUnderwritingPosition(value)
		if err != nil {
			return err
		}
		if err := tx.Table("underwriting_positions").Where("account_id = ? AND id = ?", value.AccountID, value.ID).Updates(map[string]any{
			"position_lookup": row.PositionLookup, "identity_cipher": row.IdentityCipher, "frozen_cipher": row.FrozenCipher, "status": row.Status,
		}).Error; err != nil {
			return err
		}
	}
	return nil
}

const privateEvidenceConstraints = `
ALTER TABLE missions
    ALTER COLUMN ship_lookup SET NOT NULL,
    ALTER COLUMN metadata_cipher SET NOT NULL,
    ALTER COLUMN data_cipher SET NOT NULL,
    DROP CONSTRAINT missions_pkey,
    ADD PRIMARY KEY (account_id, ship_lookup),
    ADD CHECK (length(ship_lookup) = 64),
    ADD CHECK (octet_length(metadata_cipher) >= 28 AND octet_length(data_cipher) >= 28),
    DROP COLUMN ship_id, DROP COLUMN phase, DROP COLUMN destination,
    DROP COLUMN commodity, DROP COLUMN contract_id, DROP COLUMN quantity,
    DROP COLUMN price_pence, DROP COLUMN generation, DROP COLUMN data;

DROP INDEX settlements_lane_idx;
ALTER TABLE settlements
    ALTER COLUMN lane_lookup SET NOT NULL,
    ALTER COLUMN context_cipher SET NOT NULL,
    ADD CHECK (length(lane_lookup) = 64 AND octet_length(context_cipher) >= 28),
    ADD CHECK (kind IN ('spot', 'charter', 'freight', 'spot+freight', 'charter+freight', 'recovery', 'relocation', 'aborted', 'maintenance', 'unknown')),
    DROP COLUMN ship_id, DROP COLUMN lane, DROP COLUMN origin,
    DROP COLUMN destination, DROP COLUMN commodity;
CREATE INDEX settlements_lane_idx ON settlements (account_id, lane_lookup, completed_at DESC, id);
CREATE INDEX settlements_lane_global_idx ON settlements (lane_lookup, completed_at DESC, id);

ALTER TABLE underwriting_policies
    ALTER COLUMN policy_lookup SET NOT NULL,
    ALTER COLUMN identity_cipher SET NOT NULL,
    ALTER COLUMN payload_cipher SET NOT NULL,
    DROP CONSTRAINT underwriting_policies_pkey,
    ADD PRIMARY KEY (account_id, policy_lookup),
    ADD CHECK (length(policy_lookup) = 64),
    ADD CHECK (octet_length(identity_cipher) >= 28 AND octet_length(payload_cipher) >= 28),
    ADD CHECK (status IN ('open', 'active', 'settled_safe', 'claimed_loss', 'cancelled', 'terms_changed', 'unknown')),
    DROP COLUMN policy_id, DROP COLUMN terms_hash, DROP COLUMN payload;

DROP INDEX underwriting_positions_account_accepted_idx;
ALTER TABLE underwriting_positions
    ALTER COLUMN position_lookup SET NOT NULL,
    ALTER COLUMN identity_cipher SET NOT NULL,
    ALTER COLUMN frozen_cipher SET NOT NULL,
    DROP CONSTRAINT underwriting_positions_pkey,
    ADD PRIMARY KEY (account_id, position_lookup),
    ADD CHECK (length(position_lookup) = 64),
    ADD CHECK (octet_length(identity_cipher) >= 28 AND octet_length(frozen_cipher) >= 28),
    ADD CHECK (status IN ('active', 'settled_safe', 'claimed_loss', 'cancelled', 'unknown')),
    DROP COLUMN id, DROP COLUMN policy_id, DROP COLUMN company_id, DROP COLUMN frozen_policy;
CREATE INDEX underwriting_positions_account_accepted_idx ON underwriting_positions (account_id, accepted_at, position_lookup);

CREATE OR REPLACE FUNCTION guard_underwriting_policy_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.account_id, NEW.policy_lookup, NEW.identity_cipher)
       IS DISTINCT FROM ROW(OLD.account_id, OLD.policy_lookup, OLD.identity_cipher) THEN
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
CREATE TRIGGER underwriting_policy_evidence_immutable BEFORE UPDATE ON underwriting_policies
    FOR EACH ROW EXECUTE FUNCTION guard_underwriting_policy_evidence();

CREATE OR REPLACE FUNCTION guard_underwriting_position_immutability() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.account_id, NEW.position_lookup, NEW.action_id, NEW.identity_cipher,
           NEW.slice_bps, NEW.collateral_pence, NEW.premium_pence, NEW.frozen_cipher, NEW.accepted_at)
       IS DISTINCT FROM
       ROW(OLD.account_id, OLD.position_lookup, OLD.action_id, OLD.identity_cipher,
           OLD.slice_bps, OLD.collateral_pence, OLD.premium_pence, OLD.frozen_cipher, OLD.accepted_at) THEN
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
CREATE TRIGGER underwriting_position_immutable BEFORE UPDATE ON underwriting_positions
    FOR EACH ROW EXECUTE FUNCTION guard_underwriting_position_immutability();

CREATE TABLE private_evidence_key (
    singleton boolean PRIMARY KEY CHECK (singleton),
    verifier bytea NOT NULL CHECK (octet_length(verifier) = 32)
);
`
