package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const FuturesExposureLimitPence int64 = 2_000_000

type FuturesIntent struct {
	AccountID         string          `json:"account_id" gorm:"primaryKey"`
	IntentID          string          `json:"intent_id" gorm:"primaryKey"`
	ActionID          string          `json:"action_id"`
	CohortID          string          `json:"cohort_id"`
	PortID            string          `json:"port_id"`
	CommodityID       string          `json:"commodity_id"`
	Side              string          `json:"side"`
	LotTons           int64           `json:"lot_tons"`
	StrikePricePence  int64           `json:"strike_price_pence"`
	ReservedTons      int64           `json:"reserved_tons"`
	ReservedCashPence int64           `json:"reserved_cash_pence"`
	MarginPence       int64           `json:"margin_pence"`
	ExposurePence     int64           `json:"exposure_pence"`
	Status            string          `json:"status"`
	MaturesAt         time.Time       `json:"matures_at"`
	CreatedAt         time.Time       `json:"created_at"`
	Strategy          string          `json:"strategy"`
	ListingID         string          `json:"listing_id"`
	CounterpartyID    string          `json:"counterparty_id"`
	MaturityGameHours int64           `json:"maturity_game_hours"`
	Admission         json.RawMessage `json:"admission" gorm:"type:jsonb"`
	FuturesID         string          `json:"futures_id"`
	ObservedMaturesAt *time.Time      `json:"observed_matures_at"`
	Contract          json.RawMessage `json:"contract" gorm:"type:jsonb"`
	Outcome           json.RawMessage `json:"outcome" gorm:"type:jsonb"`
	RealizedPnLPence  *int64          `json:"realized_pnl_pence" gorm:"column:realized_pnl_pence"`
	CashDeltaPence    *int64          `json:"cash_delta_pence"`
	PnLBasis          string          `json:"pnl_basis" gorm:"column:pnl_basis"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

func (FuturesIntent) TableName() string { return "futures_intents" }

func FuturesTerminal(status string) bool {
	return status == "settled" || status == "defaulted" || status == "cancelled" || status == "rejected"
}

func sameFuturesIntent(a, b FuturesIntent) bool {
	return a.AccountID == b.AccountID && a.IntentID == b.IntentID && a.ActionID == b.ActionID &&
		a.CohortID == b.CohortID && a.PortID == b.PortID && a.CommodityID == b.CommodityID &&
		a.Side == b.Side && a.LotTons == b.LotTons && a.StrikePricePence == b.StrikePricePence &&
		a.ReservedTons == b.ReservedTons && a.ReservedCashPence == b.ReservedCashPence &&
		a.MarginPence == b.MarginPence && a.ExposurePence == b.ExposurePence &&
		a.MaturesAt.Equal(b.MaturesAt) && a.CreatedAt.Equal(b.CreatedAt) &&
		a.Strategy == b.Strategy && a.ListingID == b.ListingID && a.CounterpartyID == b.CounterpartyID &&
		a.MaturityGameHours == b.MaturityGameHours && bytes.Equal(a.Admission, b.Admission)
}

// A new key is permitted only after the previous request is positively known
// not to have committed. The financial terms and the permanent cohort stay fixed.
func (s *Store) ReadmitFuturesIntent(ctx context.Context, accountID, intentID, actionID string) error {
	if !validID(actionID) {
		return ErrInvalidInput
	}
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		var row FuturesIntent
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("account_id = ? AND intent_id = ?", accountID, intentID).Take(&row).Error; err != nil {
			return err
		}
		if row.Status != "intent" || row.FuturesID != "" {
			return ErrActionConflict
		}
		if row.ActionID == actionID {
			return nil
		}
		var journal actionRow
		if err := tx.Where("account_id = ? AND id = ?", accountID, row.ActionID).Take(&journal).Error; err != nil {
			return err
		}
		previous, err := s.decodeAction(journal)
		if err != nil {
			return err
		}
		var proof struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(previous.Response, &proof)
		neverSent := previous.State == ActionReconciled &&
			(proof.Kind == "never_sent_financial_event" || proof.Kind == "never_sent_control_event" || proof.Kind == "never_sent_precondition")
		retryable := previous.State == ActionComplete && (previous.Status == 401 || previous.Status == 429)
		if !neverSent && !retryable {
			return ErrActionConflict
		}
		var group []FuturesIntent
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("account_id = ? AND action_id = ? AND cohort_id = ?", accountID, row.ActionID, row.CohortID).Find(&group).Error; err != nil {
			return err
		}
		for _, leg := range group {
			if leg.Status != "intent" || leg.FuturesID != "" {
				return ErrActionConflict
			}
		}
		return tx.Model(&FuturesIntent{}).Where("account_id = ? AND action_id = ? AND cohort_id = ?", accountID, row.ActionID, row.CohortID).
			Updates(map[string]any{"action_id": actionID, "updated_at": time.Now().UTC()}).Error
	})
}

// RejectUnsentFuturesIntent releases stopped admissions only with positive
// no-send evidence. Pending or committed requests retain their coverage.
func (s *Store) RejectUnsentFuturesIntent(ctx context.Context, accountID, intentID string) (bool, error) {
	rejected := false
	err := s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		var row FuturesIntent
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("account_id = ? AND intent_id = ?", accountID, intentID).Take(&row).Error; err != nil {
			return err
		}
		if row.Status != "intent" || row.FuturesID != "" {
			return nil
		}
		var journal actionRow
		err := tx.Where("account_id = ? AND id = ?", accountID, row.ActionID).Take(&journal).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil {
			previous, err := s.decodeAction(journal)
			if err != nil {
				return err
			}
			var proof struct {
				Kind string `json:"kind"`
			}
			if json.Unmarshal(previous.Response, &proof) != nil || previous.State != ActionReconciled ||
				(proof.Kind != "never_sent_financial_event" && proof.Kind != "never_sent_control_event" &&
					proof.Kind != "never_sent_precondition" && proof.Kind != "never_sent_futures_terms") {
				return nil
			}
		}
		rejected = true
		return tx.Model(&row).Updates(map[string]any{"status": "rejected",
			"outcome": json.RawMessage(`{"kind":"never_sent_retired_admission"}`), "updated_at": time.Now().UTC()}).Error
	})
	return rejected, err
}

// A successful private event need not repeat default-only liability fields.
// A public terminal listing alone still cannot prove physical delivery.
func FuturesSettlement(row FuturesIntent) (gameapi.SettleFuturesResult, bool) {
	var event gameapi.FuturesSettlementEvent
	if json.Unmarshal(row.Outcome, &event) != nil || event.FuturesID == "" {
		var envelope struct {
			Payload gameapi.FuturesSettlementEvent `json:"payload"`
		}
		if json.Unmarshal(row.Outcome, &envelope) != nil {
			return gameapi.SettleFuturesResult{}, false
		}
		event = envelope.Payload
	}
	receipt, complete := event.Receipt()
	valid := complete && receipt.FuturesID == row.FuturesID && receipt.Status == row.Status &&
		receipt.SettledTons >= 0 && int64(receipt.SettledTons) <= row.LotTons &&
		receipt.PenaltyPaidPence >= 0 && receipt.PenaltyPaidPence <= 1_000_000_000_000 &&
		receipt.ClaimPence >= 0 && receipt.ClaimPence <= 1_000_000_000_000 &&
		(receipt.Status != "settled" || receipt.PenaltyPaidPence == 0 && receipt.ClaimPence == 0) &&
		(event.NotionalPence == nil || *event.NotionalPence == int64(receipt.SettledTons)*row.StrikePricePence) &&
		(event.PortID == "" || event.PortID == row.PortID) &&
		(event.CommodityID == "" || event.CommodityID == row.CommodityID)
	var contract gameapi.FuturesContract
	if json.Unmarshal(row.Contract, &contract) == nil && contract.ID == row.FuturesID {
		valid = valid && (event.SellerCompanyID == nil || contract.SellerCompanyID != nil &&
			*event.SellerCompanyID == *contract.SellerCompanyID) &&
			(event.BuyerCompanyID == nil || contract.BuyerCompanyID != nil &&
				*event.BuyerCompanyID == *contract.BuyerCompanyID)
	}
	return receipt, valid
}

// PutFuturesIntent reserves immutable terms before any network send.
func (s *Store) PutFuturesIntent(ctx context.Context, intent *FuturesIntent) error {
	if intent == nil {
		return ErrInvalidInput
	}
	return s.PutFuturesIntents(ctx, []*FuturesIntent{intent})
}

// A cross pair reserves both legs in one transaction, with the same journal key.
func (s *Store) PutFuturesIntents(ctx context.Context, intents []*FuturesIntent) error {
	if len(intents) == 0 || len(intents) > 25 || intents[0] == nil {
		return ErrInvalidInput
	}
	accountID := intents[0].AccountID
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		var account Account
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&account, "id = ?", accountID).Error; err != nil {
			return err
		}
		for _, intent := range intents {
			if intent == nil || intent.AccountID != accountID || !validID(intent.IntentID) || !validID(intent.ActionID) || !validID(intent.CohortID) {
				return fmt.Errorf("futures intent identity: %w", ErrInvalidInput)
			}
			if len(intent.Admission) != 0 && !json.Valid(intent.Admission) {
				return fmt.Errorf("futures admission evidence: %w", ErrInvalidInput)
			}
			intent.CreatedAt = intent.CreatedAt.UTC().Truncate(time.Microsecond)
			intent.MaturesAt = intent.MaturesAt.UTC().Truncate(time.Microsecond)
			var existing FuturesIntent
			err := tx.Where("account_id = ? AND intent_id = ?", accountID, intent.IntentID).Take(&existing).Error
			if err == nil {
				var a, b any
				_ = json.Unmarshal(existing.Admission, &a)
				_ = json.Unmarshal(intent.Admission, &b)
				existing.Admission, _ = json.Marshal(a)
				comparable := *intent
				comparable.Admission, _ = json.Marshal(b)
				if !sameFuturesIntent(existing, comparable) {
					return ErrActionConflict
				}
				continue
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if intent.PortID == "" || intent.CommodityID == "" || intent.LotTons <= 0 ||
				intent.LotTons > 2_000_000 || intent.StrikePricePence <= 0 ||
				intent.StrikePricePence > FuturesExposureLimitPence/intent.LotTons ||
				(intent.Side != "ask" && intent.Side != "bid") {
				return fmt.Errorf("futures terms: %w", ErrInvalidInput)
			}
			notional := intent.StrikePricePence * intent.LotTons
			if intent.MarginPence != notional/4 || intent.ExposurePence <= 0 || intent.ExposurePence > FuturesExposureLimitPence ||
				intent.ReservedCashPence < intent.MarginPence || intent.ExposurePence < intent.ReservedCashPence ||
				intent.Status != "intent" || intent.CreatedAt.IsZero() || !intent.MaturesAt.After(intent.CreatedAt) ||
				intent.Side == "ask" && intent.ReservedTons != intent.LotTons ||
				intent.Side == "bid" && (intent.ReservedTons != 0 || intent.ReservedCashPence < notional+intent.MarginPence) {
				return fmt.Errorf("covered futures intent: %w", ErrInvalidInput)
			}
			row := *intent
			row.UpdatedAt = time.Now().UTC()
			row.FuturesID, row.ObservedMaturesAt, row.Contract, row.Outcome, row.RealizedPnLPence = "", nil, nil, nil, nil
			row.CashDeltaPence, row.PnLBasis = nil, ""
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) FuturesIntents(ctx context.Context, accountID string) ([]FuturesIntent, error) {
	if !validID(accountID) {
		return nil, ErrInvalidInput
	}
	db, cancel := s.query(ctx)
	defer cancel()
	rows := make([]FuturesIntent, 0)
	err := db.Where("account_id = ?", accountID).Order("created_at, intent_id").Find(&rows).Error
	return rows, err
}

type futuresCreditEvidence struct {
	AccountID         string `gorm:"primaryKey"`
	LastAttemptAt     time.Time
	AttemptedServerAt time.Time
	ObservedAt        *time.Time
	ClaimsOpenPence   *int64
}

func (futuresCreditEvidence) TableName() string { return "futures_credit_evidence" }

func futuresDefaultAt(row FuturesIntent) time.Time {
	var contract struct {
		UpdatedAt time.Time `json:"updated_at"`
	}
	var event struct {
		Timestamp time.Time `json:"timestamp"`
	}
	_ = json.Unmarshal(row.Contract, &contract)
	_ = json.Unmarshal(row.Outcome, &event)
	at := contract.UpdatedAt
	if event.Timestamp.After(at) {
		at = event.Timestamp
	}
	return at
}

func futuresCreditAfterDefault(row FuturesIntent, credit futuresCreditEvidence) bool {
	defaultAt := futuresDefaultAt(row)
	return !defaultAt.IsZero() && credit.ObservedAt != nil && credit.ClaimsOpenPence != nil && credit.ObservedAt.After(defaultAt)
}

// ReserveFuturesCreditRead persists the five-real-minute allowance before GET.
// Only unresolved default liability justifies an own-company credit observation.
// It reports whether a read was reserved and whether credit evidence is awaited.
func (s *Store) ReserveFuturesCreditRead(ctx context.Context, accountID string, serverLower time.Time) (bool, bool, error) {
	if !validID(accountID) {
		return false, false, ErrInvalidInput
	}
	db, cancel := s.query(ctx)
	defer cancel()
	var rows []FuturesIntent
	if err := db.Select("contract, outcome").Where("account_id = ? AND status = 'defaulted'", accountID).Find(&rows).Error; err != nil || len(rows) == 0 {
		return false, false, err
	}
	var credit futuresCreditEvidence
	if err := db.Where("account_id = ?", accountID).Limit(1).Find(&credit).Error; err != nil {
		return false, false, err
	}
	awaited := false
	for _, row := range rows {
		if !futuresCreditAfterDefault(row, credit) || *credit.ClaimsOpenPence != 0 {
			awaited = true
			break
		}
	}
	if !awaited || !credit.LastAttemptAt.IsZero() && time.Since(credit.LastAttemptAt) < 5*time.Minute {
		return false, awaited, nil
	}
	if serverLower.IsZero() {
		return false, true, ErrInvalidInput
	}
	reserved := false
	err := s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		var account Account
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&account, "id = ?", accountID).Error; err != nil {
			return err
		}
		if err := tx.Where("account_id = ?", accountID).Limit(1).Find(&credit).Error; err != nil {
			return err
		}
		now := time.Now().UTC()
		if !credit.LastAttemptAt.IsZero() && now.Sub(credit.LastAttemptAt) < 5*time.Minute {
			return nil
		}
		if err := tx.Select("contract, outcome").Where("account_id = ? AND status = 'defaulted'", accountID).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			if futuresCreditAfterDefault(row, credit) && *credit.ClaimsOpenPence == 0 {
				continue
			}
			reserved = true
			break
		}
		awaited = reserved
		if !reserved {
			return nil
		}
		return tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "account_id"}},
			DoUpdates: clause.Assignments(map[string]any{
				"last_attempt_at": now, "attempted_server_at": serverLower,
			}),
		}).Create(&futuresCreditEvidence{AccountID: accountID, LastAttemptAt: now, AttemptedServerAt: serverLower}).Error
	})
	return reserved && err == nil, awaited, err
}

// Invalid or failed observations never replace the last validated claim amount.
func (s *Store) RecordFuturesCredit(ctx context.Context, accountID string, raw json.RawMessage) error {
	var report struct {
		CompanyID string `json:"company_id"`
		Claims    *int64 `json:"futures_claims_open_pence"`
	}
	if json.Unmarshal(raw, &report) != nil || report.Claims == nil || *report.Claims < 0 || *report.Claims > 1_000_000_000_000 {
		return ErrInvalidInput
	}
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		var account Account
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&account, "id = ?", accountID).Error; err != nil {
			return err
		}
		if report.CompanyID != account.CompanyID {
			return ErrInvalidInput
		}
		result := tx.Model(&futuresCreditEvidence{}).Where("account_id = ?", accountID).
			Updates(map[string]any{"observed_at": gorm.Expr("attempted_server_at"), "claims_open_pence": *report.Claims})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrInvalidInput
		}
		return nil
	})
}

// Cash already moved into escrow is not reserved a second time in the vault.
// Unknown creates retain their entire reserve until positive outcome evidence.
func (s *Store) FuturesCashReserve(ctx context.Context, accountID string) (int64, error) {
	reserves, err := s.FuturesCashReserveByRegion(ctx, accountID, nil)
	var total int64
	for _, reserve := range reserves {
		total += reserve
	}
	return total, err
}

func (s *Store) FuturesCashReserveByRegion(ctx context.Context, accountID string, portRegions map[string]string) (map[string]int64, error) {
	rows, err := s.FuturesIntents(ctx, accountID)
	if err != nil {
		return nil, err
	}
	db, cancel := s.query(ctx)
	defer cancel()
	var credit futuresCreditEvidence
	if err := db.Where("account_id = ?", accountID).Limit(1).Find(&credit).Error; err != nil {
		return nil, err
	}
	var paid []string
	hasDefaults := false
	for i := range rows {
		if rows[i].Status == "defaulted" {
			hasDefaults = true
			break
		}
	}
	if hasDefaults {
		if err := db.Table("futures_cash_flows AS flow").Distinct("flow.intent_id").
			Joins("JOIN futures_intents AS intent ON intent.account_id = flow.account_id AND intent.intent_id = flow.intent_id").
			Where("flow.account_id = ? AND intent.status = 'defaulted' AND flow.cash_delta_pence < 0", accountID).
			Where("LOWER(flow.evidence ->> 'reason') LIKE '%penalty%' AND flow.evidence #>> '{metadata,futures_id}' = intent.futures_id").
			Pluck("flow.intent_id", &paid).Error; err != nil {
			return nil, err
		}
	}
	cash := make(map[string]int64)
	claimRegions := make(map[string]bool)
	for _, row := range rows {
		region := portRegions[row.PortID]
		if region == "" {
			region = "north_sea"
		}
		if FuturesTerminal(row.Status) && row.Status != "defaulted" {
			continue
		}
		reserved := row.ReservedCashPence
		if row.FuturesID != "" {
			reserved -= row.MarginPence
		}
		reserved = max(0, reserved)
		if row.Status != "defaulted" {
			cash[region] += reserved
			continue
		}
		if futuresCreditAfterDefault(row, credit) {
			// The aggregate replaces per-contract estimates, not the debt itself.
			// Positive claims remain protected until a later valid reduction.
			if credit.ClaimsOpenPence != nil && *credit.ClaimsOpenPence > 0 {
				claimRegions[region] = true
			}
			continue
		}
		// Conservative unattributed reserve: a penalty debit identifies our
		// default, but does not pay the separate unpaid claim.
		identified := false
		for _, intentID := range paid {
			if intentID == row.IntentID {
				identified = true
				break
			}
		}
		if receipt, known := FuturesSettlement(row); known {
			if identified && receipt.ClaimPence > 0 {
				reserved = receipt.ClaimPence
			} else {
				reserved = max(reserved, receipt.ClaimPence)
			}
		}
		cash[region] += reserved
	}
	if credit.ClaimsOpenPence != nil {
		if len(claimRegions) == 0 {
			claimRegions["north_sea"] = true
		}
		// The own-credit aggregate has no regional breakdown. Preserve its
		// full amount in every possible billing region rather than inventing
		// an allocation that could free an indebted hub's trading cash.
		for region := range claimRegions {
			cash[region] += *credit.ClaimsOpenPence
		}
	}
	return cash, nil
}

func (s *Store) ObserveFuturesContract(ctx context.Context, accountID, intentID string, contract gameapi.FuturesContract) error {
	if !validID(contract.ID) || contract.MaturesAt.IsZero() || contract.CreatedAt.IsZero() ||
		(contract.Side != "ask" && contract.Side != "bid") ||
		!contract.MaturesAt.After(contract.CreatedAt) || contract.StrikePricePence <= 0 ||
		contract.SellerMarginPence < 0 || contract.BuyerMarginPence < 0 ||
		(contract.Status != "open" && contract.Status != "active" && contract.Status != "settled" &&
			contract.Status != "defaulted" && contract.Status != "cancelled") {
		// These are all FuturesStatus values in the current OpenAPI schema.
		return ErrInvalidInput
	}
	secrets, err := s.secrets(ctx, accountID)
	if err != nil {
		return err
	}
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		var row FuturesIntent
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("account_id = ? AND intent_id = ?", accountID, intentID).Take(&row).Error; err != nil {
			return err
		}
		var account Account
		if err := tx.First(&account, "id = ?", accountID).Error; err != nil {
			return err
		}
		creating := row.ListingID == ""
		if row.FuturesID != "" && row.FuturesID != contract.ID ||
			creating && (contract.CreatorCompanyID != account.CompanyID || contract.Side != row.Side) ||
			!creating && (contract.ID != row.ListingID || contract.CreatorCompanyID != row.CounterpartyID || contract.Side == row.Side) ||
			contract.PortID != row.PortID || contract.CommodityID != row.CommodityID ||
			int64(contract.LotTons) != row.LotTons || contract.StrikePricePence != row.StrikePricePence ||
			!creating && !contract.MaturesAt.Equal(row.MaturesAt) {
			return ErrActionConflict
		}
		party, counterparty, margin := contract.SellerCompanyID, contract.BuyerCompanyID, contract.SellerMarginPence
		if row.Side == "bid" {
			party, counterparty, margin = contract.BuyerCompanyID, contract.SellerCompanyID, contract.BuyerMarginPence
		}
		if party == nil || *party != account.CompanyID || margin < 0 || margin > row.MarginPence ||
			!creating && (counterparty == nil || *counterparty != row.CounterpartyID) {
			return ErrActionConflict
		}
		if FuturesTerminal(row.Status) && row.Status != "rejected" && row.Status != contract.Status {
			return nil // A late listing must not resurrect a permanent terminal receipt.
		}
		body, err := json.Marshal(contract)
		if err != nil {
			return err
		}
		body, err = redactJSON(BoundEventPayload(body), secrets, maxDiagnosticBytes)
		if err != nil {
			return err
		}
		updates := map[string]any{"futures_id": contract.ID, "status": contract.Status,
			"observed_matures_at": contract.MaturesAt, "contract": body, "updated_at": time.Now().UTC()}
		if row.Status == "rejected" {
			updates["outcome"] = nil
			updates["realized_pnl_pence"], updates["cash_delta_pence"], updates["pnl_basis"] = nil, nil, ""
		}
		return tx.Model(&row).Where(`futures_id IS DISTINCT FROM ? OR status IS DISTINCT FROM ?
			OR observed_matures_at IS DISTINCT FROM ? OR contract IS DISTINCT FROM ?::jsonb`,
			contract.ID, contract.Status, contract.MaturesAt, string(body)).
			Updates(updates).Error
	})
}

func (s *Store) RecordFuturesOutcome(ctx context.Context, accountID, intentID, status string, evidence json.RawMessage) error {
	if !FuturesTerminal(status) || !json.Valid(evidence) || bytes.Equal(bytes.TrimSpace(evidence), []byte("null")) {
		return ErrInvalidInput
	}
	secrets, err := s.secrets(ctx, accountID)
	if err != nil {
		return err
	}
	evidence, err = redactJSON(BoundEventPayload(evidence), secrets, maxDiagnosticBytes)
	if err != nil {
		return err
	}
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		var row FuturesIntent
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("account_id = ? AND intent_id = ?", accountID, intentID).Take(&row).Error; err != nil {
			return err
		}
		if FuturesTerminal(row.Status) && row.Status != status {
			return ErrActionConflict
		}
		return tx.Model(&row).Where("status IS DISTINCT FROM ? OR outcome IS DISTINCT FROM ?::jsonb", status, string(evidence)).
			Updates(map[string]any{"status": status, "outcome": []byte(evidence), "updated_at": time.Now().UTC()}).Error
	})
}

func (s *Store) FuturesEvents(ctx context.Context, accountID string) ([]Event, error) {
	if !validID(accountID) {
		return nil, ErrInvalidInput
	}
	db, cancel := s.query(ctx)
	defer cancel()
	var rows []Event
	err := db.Where("account_id = ? AND event_type IN ?", accountID,
		[]string{"futures_settled", "futures_defaulted", "futures_expired"}).Order("id").Find(&rows).Error
	return rows, err
}

type futuresCashFlow struct {
	AccountID      string `gorm:"primaryKey"`
	IntentID       string
	LedgerID       string `gorm:"primaryKey"`
	CashDeltaPence int64
	Evidence       json.RawMessage `gorm:"type:jsonb"`
}

func (futuresCashFlow) TableName() string { return "futures_cash_flows" }

// Retain attributed cash movements independently of the seven-day REST log.
// Signed deltas include our vault and escrow, so returned margin is not profit.
func (s *Store) RecordFuturesCashFlow(ctx context.Context, accountID, intentID, ledgerID string, delta int64, evidence json.RawMessage) error {
	if !validID(accountID) || !validID(intentID) || !validID(ledgerID) || !json.Valid(evidence) {
		return ErrInvalidInput
	}
	secrets, err := s.secrets(ctx, accountID)
	if err != nil {
		return err
	}
	evidence, err = redactJSON(BoundEventPayload(evidence), secrets, maxDiagnosticBytes)
	if err != nil {
		return err
	}
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		row := futuresCashFlow{AccountID: accountID, IntentID: intentID, LedgerID: ledgerID, CashDeltaPence: delta, Evidence: evidence}
		created := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		if created.Error != nil {
			return created.Error
		}
		var existing futuresCashFlow
		if err := tx.Where("account_id = ? AND ledger_id = ?", accountID, ledgerID).Take(&existing).Error; err != nil {
			return err
		}
		if existing.IntentID != intentID || existing.CashDeltaPence != delta {
			return ErrActionConflict
		}
		if created.RowsAffected == 0 {
			return nil
		}
		// Cash receipts are not sale profit without the replacement inventory
		// cost. Keep the signed cash measurement distinct from realized P&L.
		return tx.Exec(`UPDATE futures_intents SET cash_delta_pence =
			(SELECT COALESCE(SUM(cash_delta_pence), 0) FROM futures_cash_flows WHERE account_id = ? AND intent_id = ?)
			WHERE account_id = ? AND intent_id = ?`,
			accountID, intentID, accountID, intentID).Error
	})
}

type FuturesCohortAccounting struct {
	ProcurementPence int64
	LiquidationPence int64
	LiquidatedTons   int64
	SetupOwned       bool
	WindDownComplete bool
	CashUnknown      bool
}

// Cash becomes realized P&L only when the physical stock cycle is closed. An
// unchanged external opening ton is a replacement-cost pair; an unpriced ton
// sold without replacement remains explicitly unknown, never gross-sale profit.
func (s *Store) RecordFuturesPairPnL(ctx context.Context, accountID, cohortID string, accounting FuturesCohortAccounting) error {
	if !validID(accountID) || !validID(cohortID) || accounting.ProcurementPence < 0 ||
		accounting.LiquidationPence < 0 || accounting.LiquidatedTons < 0 {
		return ErrInvalidInput
	}
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		var rows []FuturesIntent
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("account_id = ? AND cohort_id = ?", accountID, cohortID).
			Order("side, intent_id").Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 || accounting.SetupOwned && !accounting.WindDownComplete {
			return nil
		}
		pnl := accounting.LiquidationPence - accounting.ProcurementPence
		stock := -accounting.LiquidatedTons
		if accounting.ProcurementPence > 0 {
			stock++
		}
		for _, row := range rows {
			if !FuturesTerminal(row.Status) {
				return nil
			}
			if row.Status == "settled" || row.Status == "defaulted" {
				receipt, known := FuturesSettlement(row)
				if !known || row.CashDeltaPence == nil {
					return nil
				}
				notional := int64(receipt.SettledTons) * row.StrikePricePence
				if row.Status == "settled" && (row.Side == "ask" && *row.CashDeltaPence < notional ||
					row.Side == "bid" && *row.CashDeltaPence > -notional) {
					return nil
				}
				if row.Side == "bid" {
					stock += int64(receipt.SettledTons)
				} else {
					stock -= int64(receipt.SettledTons)
				}
			} else if row.FuturesID != "" && row.CashDeltaPence == nil {
				return nil
			}
			if row.CashDeltaPence != nil {
				pnl += *row.CashDeltaPence
			}
		}
		basis := "measured cohort cash after procurement and liquidation; rent unknown; unpaid default claims excluded"
		for i, row := range rows {
			var value *int64
			if accounting.CashUnknown {
				basis = "procurement or liquidation cash unknown; rent unknown"
			} else if stock == 0 {
				amount := int64(0)
				if i == 0 {
					amount = pnl
				}
				value = &amount
			} else {
				basis = "opening or remaining stock cost unknown; rent unknown"
			}
			if row.PnLBasis == basis && (row.RealizedPnLPence == nil && value == nil ||
				row.RealizedPnLPence != nil && value != nil && *row.RealizedPnLPence == *value) {
				continue
			}
			if err := tx.Model(&row).Updates(map[string]any{"realized_pnl_pence": value, "pnl_basis": basis}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
