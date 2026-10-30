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

const FuturesExposureLimitPence int64 = 30_000

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
		a.MaturesAt.Equal(b.MaturesAt) && a.CreatedAt.Equal(b.CreatedAt)
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
		retryable := previous.State == ActionComplete && (previous.Status == 401 || previous.Status == 403 || previous.Status == 429)
		if !neverSent && !retryable {
			return ErrActionConflict
		}
		return tx.Model(&row).Updates(map[string]any{"action_id": actionID, "updated_at": time.Now().UTC()}).Error
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
			"outcome": json.RawMessage(`{"kind":"never_sent_disabled_admission"}`), "updated_at": time.Now().UTC()}).Error
	})
	return rejected, err
}

// FuturesSettlement returns only complete physical and default-liability evidence.
// A terminal public listing alone does not supply these accounting facts.
func FuturesSettlement(row FuturesIntent) (gameapi.SettleFuturesResult, bool) {
	var receipt gameapi.SettleFuturesResult
	if json.Unmarshal(row.Outcome, &receipt) != nil || receipt.FuturesID == "" {
		var event struct {
			Payload gameapi.SettleFuturesResult `json:"payload"`
		}
		if json.Unmarshal(row.Outcome, &event) != nil {
			return receipt, false
		}
		receipt = event.Payload
	}
	valid := receipt.FuturesID == row.FuturesID && receipt.Status == row.Status &&
		(receipt.Status == "settled" || receipt.Status == "defaulted") &&
		receipt.SettledTons >= 0 && int64(receipt.SettledTons) <= row.LotTons &&
		receipt.PenaltyPaidPence >= 0 && receipt.PenaltyPaidPence <= 1_000_000_000_000 &&
		receipt.ClaimPence >= 0 && receipt.ClaimPence <= 1_000_000_000_000
	return receipt, valid
}

// PutFuturesIntent admits immutable terms before any network send. Locking the
// account row serializes the cap check, including callers without a runner lease.
func (s *Store) PutFuturesIntent(ctx context.Context, intent *FuturesIntent) error {
	if intent == nil || !validID(intent.AccountID) || !validID(intent.IntentID) || !validID(intent.ActionID) || !validID(intent.CohortID) {
		return fmt.Errorf("futures intent identity: %w", ErrInvalidInput)
	}
	invalidTerms := intent.PortID != "amsterdam" || intent.CommodityID != "water_beer" || intent.LotTons != 1 ||
		(intent.Side != "ask" && intent.Side != "bid") || intent.StrikePricePence <= 0 || intent.StrikePricePence > FuturesExposureLimitPence ||
		intent.MarginPence != intent.StrikePricePence/4 || intent.ExposurePence <= 0 || intent.ExposurePence > FuturesExposureLimitPence ||
		intent.ReservedCashPence < intent.MarginPence || intent.ExposurePence < intent.ReservedCashPence || intent.ReservedTons < 0 || intent.Status != "intent" ||
		intent.CreatedAt.IsZero() || !intent.MaturesAt.After(intent.CreatedAt) ||
		intent.Side == "ask" && intent.ReservedTons != 1 || intent.Side == "bid" && (intent.ReservedTons != 0 || intent.ReservedCashPence < intent.StrikePricePence+intent.MarginPence)
	intent.CreatedAt = intent.CreatedAt.UTC().Truncate(time.Microsecond)
	intent.MaturesAt = intent.MaturesAt.UTC().Truncate(time.Microsecond)
	return s.writeOwned(ctx, intent.AccountID, func(tx *gorm.DB) error {
		var account Account
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&account, "id = ?", intent.AccountID).Error; err != nil {
			return err
		}
		var existing FuturesIntent
		err := tx.Where("account_id = ? AND (intent_id = ? OR action_id = ?)", intent.AccountID, intent.IntentID, intent.ActionID).Take(&existing).Error
		if err == nil {
			if !sameFuturesIntent(existing, *intent) {
				return ErrActionConflict
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if invalidTerms {
			return fmt.Errorf("covered futures intent: %w", ErrInvalidInput)
		}
		var exposure int64
		if err := tx.Model(&FuturesIntent{}).Select("COALESCE(SUM(exposure_pence), 0)").
			Where("account_id = ? AND status NOT IN ?", intent.AccountID, []string{"settled", "cancelled", "rejected"}).Scan(&exposure).Error; err != nil {
			return err
		}
		if exposure > FuturesExposureLimitPence-intent.ExposurePence {
			return fmt.Errorf("futures exposure exceeds %dp: %w", FuturesExposureLimitPence, ErrInvalidInput)
		}
		row := *intent
		row.UpdatedAt = time.Now().UTC()
		row.FuturesID, row.ObservedMaturesAt, row.Contract, row.Outcome, row.RealizedPnLPence = "", nil, nil, nil, nil
		row.CashDeltaPence, row.PnLBasis = nil, ""
		return tx.Create(&row).Error
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
	rows, err := s.FuturesIntents(ctx, accountID)
	if err != nil {
		return 0, err
	}
	db, cancel := s.query(ctx)
	defer cancel()
	var credit futuresCreditEvidence
	if err := db.Where("account_id = ?", accountID).Limit(1).Find(&credit).Error; err != nil {
		return 0, err
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
			return 0, err
		}
	}
	var cash, conservativeDefaults int64
	for _, row := range rows {
		if FuturesTerminal(row.Status) && row.Status != "defaulted" {
			continue
		}
		reserved := row.ReservedCashPence
		if row.FuturesID != "" {
			reserved -= row.MarginPence
		}
		reserved = max(0, reserved)
		if row.Status != "defaulted" {
			cash += reserved
			continue
		}
		if futuresCreditAfterDefault(row, credit) {
			// The aggregate replaces per-contract estimates, not the debt itself.
			// Positive claims remain protected until a later valid reduction.
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
		conservativeDefaults += reserved
	}
	if credit.ClaimsOpenPence != nil {
		conservativeDefaults += *credit.ClaimsOpenPence
	}
	return cash + conservativeDefaults, nil
}

func (s *Store) ObserveFuturesContract(ctx context.Context, accountID, intentID string, contract gameapi.FuturesContract) error {
	if !validID(contract.ID) || contract.MaturesAt.IsZero() || contract.CreatedAt.IsZero() ||
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
		if row.FuturesID != "" && row.FuturesID != contract.ID || contract.CreatorCompanyID != account.CompanyID ||
			contract.PortID != row.PortID || contract.CommodityID != row.CommodityID || contract.Side != row.Side ||
			int64(contract.LotTons) != row.LotTons || contract.StrikePricePence != row.StrikePricePence {
			return ErrActionConflict
		}
		party, margin := contract.SellerCompanyID, contract.SellerMarginPence
		if row.Side == "bid" {
			party, margin = contract.BuyerCompanyID, contract.BuyerMarginPence
		}
		if party == nil || *party != account.CompanyID || margin < 0 || margin > row.MarginPence {
			return ErrActionConflict
		}
		if FuturesTerminal(row.Status) && row.Status != contract.Status {
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
		return tx.Model(&row).Where(`futures_id IS DISTINCT FROM ? OR status IS DISTINCT FROM ?
			OR observed_matures_at IS DISTINCT FROM ? OR contract IS DISTINCT FROM ?::jsonb`,
			contract.ID, contract.Status, contract.MaturesAt, string(body)).
			Updates(map[string]any{"futures_id": contract.ID, "status": contract.Status,
				"observed_matures_at": contract.MaturesAt, "contract": body, "updated_at": time.Now().UTC()}).Error
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
				if row.Status == "settled" && (row.Side == "ask" && *row.CashDeltaPence < row.StrikePricePence ||
					row.Side == "bid" && *row.CashDeltaPence > -row.StrikePricePence) {
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
