package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"gorm.io/gorm"
)

type LoanReport struct {
	UpdatedAt time.Time     `json:"updated_at"`
	Accounts  []LoanAccount `json:"accounts"`
}

type LoanAccount struct {
	AccountID          string      `json:"account_id"`
	Label              string      `json:"label"`
	CompanyID          string      `json:"company_id"`
	ObservedAt         *time.Time  `json:"observed_at"`
	HistoryRefreshedAt *time.Time  `json:"history_refreshed_at"`
	HistoryAttemptedAt *time.Time  `json:"history_attempted_at"`
	HistoryComplete    bool        `json:"history_complete"`
	HistoryError       string      `json:"history_error"`
	RecordingError     string      `json:"recording_error"`
	LoansStale         bool        `json:"loans_stale"`
	LoansError         string      `json:"loans_error"`
	ActiveLoans        []LoanEntry `json:"active_loans"`
	ClosedLoans        []LoanEntry `json:"closed_loans"`
	Totals             LoanTotals  `json:"totals"`
}

type LoanTotals struct {
	OwedPence                  int64 `json:"owed_pence"`
	PrincipalRepaidPence       int64 `json:"principal_repaid_pence"`
	InterestPaidPence          int64 `json:"interest_paid_pence"`
	LedgerPrincipalRepaidPence int64 `json:"ledger_principal_repaid_pence"`
	LedgerInterestPaidPence    int64 `json:"ledger_interest_paid_pence"`
	PrincipalDifferencePence   int64 `json:"principal_difference_pence"`
	InterestDifferencePence    int64 `json:"interest_difference_pence"`
	RepaidCount                int   `json:"repaid_count"`
}

type LoanEntry struct {
	gameapi.Loan
	LenderName                   string    `json:"lender_name"`
	ObservedAt                   time.Time `json:"observed_at"`
	RemainingInstallments        *int      `json:"remaining_installments"`
	RemainingInstallmentsDerived bool      `json:"remaining_installments_derived"`
	InstallmentNote              string    `json:"installment_note"`
}

// Loan's custom decoder would otherwise be promoted and discard entry metadata.
func (entry *LoanEntry) UnmarshalJSON(raw []byte) error {
	var loan gameapi.Loan
	if err := json.Unmarshal(raw, &loan); err != nil {
		return err
	}
	var details struct {
		LenderName                   string    `json:"lender_name"`
		ObservedAt                   time.Time `json:"observed_at"`
		RemainingInstallments        *int      `json:"remaining_installments"`
		RemainingInstallmentsDerived bool      `json:"remaining_installments_derived"`
		InstallmentNote              string    `json:"installment_note"`
	}
	if err := json.Unmarshal(raw, &details); err != nil {
		return err
	}
	*entry = LoanEntry{
		Loan: loan, LenderName: details.LenderName, ObservedAt: details.ObservedAt,
		RemainingInstallments:        details.RemainingInstallments,
		RemainingInstallmentsDerived: details.RemainingInstallmentsDerived, InstallmentNote: details.InstallmentNote,
	}
	return nil
}

type loanDatasetMeta struct {
	UpdatedAt   time.Time `json:"updated_at"`
	AttemptedAt time.Time `json:"attempted_at"`
	Complete    bool      `json:"complete"`
	Stale       bool      `json:"stale"`
	Error       string    `json:"error"`
}

func (meta loanDatasetMeta) usable() bool {
	return meta.Complete && !meta.Stale && meta.Error == "" && !meta.UpdatedAt.IsZero()
}

const maxLoanLenderNameBytes = 255

const loansSnapshotProjection = `jsonb_build_object(
	'loans', data -> 'loans', 'clock', data -> 'clock',
	'_meta', jsonb_build_object('loans', data #> '{_meta,loans}',
		'loan_history', data #> '{_meta,loan_history}',
		'loan_history_recording', data #> '{_meta,loan_history_recording}', 'clock', data #> '{_meta,clock}'))`

// Loans reads only recorded observations. Loan rows contain lifetime repayments;
// company_ledger_flows is a separate, partial operating-ledger cross-check, not
// replacement authority. Its capital filter normally excludes principal and its
// interest coverage begins with retained-ledger backfill, not loan origination.
func (s *Store) Loans(ctx context.Context, accountID string) (LoanReport, error) {
	if accountID != "" && !validID(accountID) {
		return LoanReport{}, fmt.Errorf("loans query: %w", ErrInvalidInput)
	}
	report := LoanReport{UpdatedAt: time.Now().UTC(), Accounts: make([]LoanAccount, 0)}
	db, cancel := s.query(ctx)
	defer cancel()
	err := db.Transaction(func(tx *gorm.DB) error {
		query := tx.Order("created_at, id").Limit(maxDashboardAccounts + 1)
		if accountID != "" {
			query = query.Where("id = ?", accountID)
		}
		var accounts []Account
		if err := query.Find(&accounts).Error; err != nil {
			return err
		}
		if len(accounts) > maxDashboardAccounts {
			return ErrResponseTooLarge
		}
		if len(accounts) == 0 {
			if accountID != "" {
				return ErrNotFound
			}
			return nil
		}
		ids := make([]string, len(accounts))
		for index, account := range accounts {
			ids[index] = account.ID
		}
		nameBudget, err := preflightLoanReport(tx, ids)
		if err != nil {
			return err
		}
		var stored []loanHistoryRow
		if err := tx.Where("account_id IN ?", ids).Order("account_id, loan_id").Find(&stored).Error; err != nil {
			return err
		}
		byAccount := make(map[string][]loanHistoryRow, len(accounts))
		for _, row := range stored {
			byAccount[row.AccountID] = append(byAccount[row.AccountID], row)
		}
		var snapshots []snapshotRow
		if err := tx.Table("snapshots").Select("account_id, updated_at, "+loansSnapshotProjection+" AS data").
			Where("account_id IN ?", ids).Find(&snapshots).Error; err != nil {
			return err
		}
		snapshotByAccount := make(map[string]snapshotRow, len(snapshots))
		for _, snapshot := range snapshots {
			snapshotByAccount[snapshot.AccountID] = snapshot
		}
		var ledger []struct {
			AccountID string
			Principal int64
			Interest  int64
		}
		if err := tx.Table("company_ledger_flows").Select(`account_id,
			COALESCE(-sum(amount_pence) FILTER (WHERE reason = 'loan_principal_repayment' AND amount_pence < 0), 0) AS principal,
			COALESCE(-sum(amount_pence) FILTER (WHERE reason = 'loan_interest_payment' AND amount_pence < 0), 0) AS interest`).
			Where("account_id IN ? AND reason IN ?", ids, []string{"loan_principal_repayment", "loan_interest_payment"}).
			Group("account_id").Scan(&ledger).Error; err != nil {
			return err
		}
		ledgerByAccount := make(map[string]LoanTotals, len(ledger))
		for _, row := range ledger {
			ledgerByAccount[row.AccountID] = LoanTotals{LedgerPrincipalRepaidPence: row.Principal, LedgerInterestPaidPence: row.Interest}
		}
		scrubber := displayScrubber{store: s, tx: tx}
		lenders := make(map[string]bool)
		for _, account := range accounts {
			credentials := scrubber.credentials(account.ID)
			result, err := loanAccountReport(account, byAccount[account.ID], snapshotByAccount[account.ID], ledgerByAccount[account.ID], credentials)
			if err != nil {
				return err
			}
			for _, entries := range [][]LoanEntry{result.ActiveLoans, result.ClosedLoans} {
				for _, entry := range entries {
					if entry.LenderCompanyID != "" {
						lenders[entry.LenderCompanyID] = true
					}
				}
			}
			report.Accounts = append(report.Accounts, result)
		}
		names, err := recordedLoanLenders(tx, lenders)
		if err != nil {
			return err
		}
		for index := range report.Accounts {
			account := &report.Accounts[index]
			credentials := scrubber.credentials(account.AccountID)
			for _, entries := range [][]LoanEntry{account.ActiveLoans, account.ClosedLoans} {
				for entryIndex := range entries {
					entry := &entries[entryIndex]
					name := names[entry.LenderCompanyID]
					if strings.TrimSpace(name) == "" {
						id := entry.LenderCompanyID
						name = "Lender " + boundedText(id[max(0, len(id)-12):], 12)
						if entry.LenderCompanyID == "" {
							name = "Unknown lender"
						}
					}
					entry.LenderName = boundedText(displayText(name, credentials), maxLoanLenderNameBytes)
					// JSON escaping can expand each byte to six bytes. Charge
					// every repeated name before the handler marshals the report.
					nameBudget -= int64(len(entry.LenderName) * 6)
					if nameBudget < 0 {
						return ErrResponseTooLarge
					}
				}
			}
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return LoanReport{}, err
	}
	return report, nil
}

func loanAccountReport(account Account, stored []loanHistoryRow, snapshot snapshotRow, ledger LoanTotals, credentials displayCredentials) (LoanAccount, error) {
	result := LoanAccount{
		AccountID: account.ID, Label: displayText(account.Label, credentials), CompanyID: displayText(account.CompanyID, credentials),
		LoansStale: true, ActiveLoans: make([]LoanEntry, 0), ClosedLoans: make([]LoanEntry, 0),
	}
	if !credentials.available {
		result.LoansError = "Account credentials unavailable; retained loan data cannot be safely displayed."
		result.HistoryError = result.LoansError
		return result, nil
	}
	var data struct {
		Loans    json.RawMessage            `json:"loans"`
		Clock    json.RawMessage            `json:"clock"`
		Metadata map[string]loanDatasetMeta `json:"_meta"`
	}
	if len(snapshot.Data) > 0 && json.Unmarshal(snapshot.Data, &data) != nil {
		return result, errors.New("decode recorded loan snapshot")
	}
	meta := data.Metadata["loans"]
	history := data.Metadata["loan_history"]
	result.ObservedAt = loanTime(meta.UpdatedAt)
	result.HistoryRefreshedAt = loanTime(history.UpdatedAt)
	result.HistoryAttemptedAt = loanTime(history.AttemptedAt)
	result.HistoryComplete = history.usable()
	result.HistoryError = displayText(history.Error, credentials)
	result.RecordingError = displayText(data.Metadata["loan_history_recording"].Error, credentials)
	if !result.HistoryComplete && result.HistoryError == "" {
		result.HistoryError = "A complete borrower loan history has not been recorded."
	}
	result.LoansStale = !meta.usable()
	result.LoansError = displayText(meta.Error, credentials)
	if result.LoansStale && result.LoansError == "" {
		result.LoansError = "A complete current borrower loan observation is unavailable."
	}
	var clock gameapi.Clock
	if data.Metadata["clock"].usable() {
		_ = json.Unmarshal(data.Clock, &clock)
	}
	entries := make(map[string]LoanEntry, len(stored))
	for _, row := range stored {
		loan, err := displayedLoan(row.Data, credentials)
		if err != nil {
			return result, err
		}
		if loan.BorrowerCompanyID != "" && loan.BorrowerCompanyID != account.CompanyID || loanUnfundedStatus(loan.Status) {
			continue
		}
		entries[loan.ID] = LoanEntry{Loan: loan, ObservedAt: row.ObservedAt}
	}
	var current []json.RawMessage
	if len(data.Loans) > 0 && string(data.Loans) != "null" {
		if err := json.Unmarshal(data.Loans, &current); err != nil {
			return result, errors.New("decode recorded borrower loan list")
		}
	}
	if current == nil {
		result.LoansStale = true
		if result.LoansError == "" {
			result.LoansError = "The current borrower loan list has not been recorded."
		}
	}
	if result.ObservedAt == nil && current != nil {
		result.ObservedAt = loanTime(snapshot.UpdatedAt)
	}
	seenCurrent := make(map[string]bool, len(current))
	for _, raw := range current {
		loan, err := displayedLoan(raw, credentials)
		if err != nil {
			return result, err
		}
		if loan.BorrowerCompanyID != account.CompanyID || loanUnfundedStatus(loan.Status) || !validID(loan.ID) {
			continue
		}
		seenCurrent[loan.ID] = true
		observedAt := meta.UpdatedAt
		if observedAt.IsZero() {
			observedAt = snapshot.UpdatedAt
		}
		previous, exists := entries[loan.ID]
		if !exists || loanObservationNewer(loan, previous.Loan, observedAt, previous.ObservedAt) {
			entries[loan.ID] = LoanEntry{Loan: loan, ObservedAt: observedAt}
		}
	}
	result.Totals = ledger
	for _, entry := range entries {
		if !loanFundedStatus(entry.Status) {
			result.HistoryComplete = false
			if result.HistoryError == "" {
				result.HistoryError = "Some recorded loans have unrecognized statuses; their debt and repayment totals are not covered."
			}
			continue
		}
		entry.RemainingInstallments, entry.RemainingInstallmentsDerived, entry.InstallmentNote = remainingLoanInstallments(entry.Loan, clock)
		if loanOpenStatus(entry.Status) {
			if !meta.UpdatedAt.IsZero() && current != nil && !seenCurrent[entry.ID] && !meta.UpdatedAt.Before(entry.ObservedAt) {
				// The retained last-good debt list controls membership even
				// after a later refresh fails. Keep known repayments without
				// reviving its former balance until a closed-row backfill.
				result.HistoryComplete = false
				if result.HistoryError == "" {
					result.HistoryError = "Formerly open loans are absent from current debt; closed history and lifetime repayment totals await a full refresh."
				}
				if err := addLoanPence(&result.Totals.PrincipalRepaidPence, entry.PrincipalRepaidPence); err != nil {
					return result, err
				}
				if err := addLoanPence(&result.Totals.InterestPaidPence, entry.InterestPaidPence); err != nil {
					return result, err
				}
				continue
			}
			result.ActiveLoans = append(result.ActiveLoans, entry)
			if result.ObservedAt == nil || entry.ObservedAt.Before(*result.ObservedAt) {
				result.ObservedAt = loanTime(entry.ObservedAt)
			}
			if err := addLoanPence(&result.Totals.OwedPence, entry.OwedPence); err != nil {
				return result, err
			}
		} else {
			result.ClosedLoans = append(result.ClosedLoans, entry)
			if entry.Status == "repaid" && entry.OwedPence == 0 {
				result.Totals.RepaidCount++
			}
		}
		if err := addLoanPence(&result.Totals.PrincipalRepaidPence, entry.PrincipalRepaidPence); err != nil {
			return result, err
		}
		if err := addLoanPence(&result.Totals.InterestPaidPence, entry.InterestPaidPence); err != nil {
			return result, err
		}
	}
	result.Totals.PrincipalDifferencePence = result.Totals.PrincipalRepaidPence - result.Totals.LedgerPrincipalRepaidPence
	result.Totals.InterestDifferencePence = result.Totals.InterestPaidPence - result.Totals.LedgerInterestPaidPence
	sort.Slice(result.ActiveLoans, func(i, j int) bool { return result.ActiveLoans[i].ID < result.ActiveLoans[j].ID })
	sort.Slice(result.ClosedLoans, func(i, j int) bool { return result.ClosedLoans[i].ID < result.ClosedLoans[j].ID })
	return result, nil
}

func preflightLoanReport(tx *gorm.DB, accountIDs []string) (int64, error) {
	var encodedBytes int64
	// Like route storage, guard numeric expansion before rendering any JSONB
	// text, including retained snapshots written before this history existed.
	err := tx.Raw(`WITH sources AS (
		SELECT data FROM loan_history WHERE account_id IN ?
		UNION ALL
		SELECT `+loansSnapshotProjection+` AS data FROM snapshots WHERE account_id IN ?
	), sizes AS MATERIALIZED (
		SELECT CASE
			WHEN pg_column_size(data) > ? THEN ?::bigint
			WHEN EXISTS (
				SELECT 1 FROM jsonb_path_query(data, ?::jsonpath) AS numbers(value)
				WHERE CASE
					WHEN abs(value::numeric) >= 1e64 OR scale(value::numeric) > 64 THEN true
					ELSE length(value::text) > 64
				END
			) THEN ?::bigint
			ELSE octet_length(data::text)::bigint
		END AS size_bytes FROM sources
	) SELECT COALESCE(sum(size_bytes), 0) FROM sizes`,
		accountIDs, accountIDs, maxDashboardSnapshotBytes, maxDashboardSnapshotBytes+1,
		`strict $.** ? (@.type() == "number")`, maxDashboardSnapshotBytes+1).Scan(&encodedBytes).Error
	if err != nil {
		return 0, err
	}
	if encodedBytes > maxDashboardSnapshotBytes {
		return 0, ErrResponseTooLarge
	}
	return maxDashboardSnapshotBytes - encodedBytes, nil
}

func displayedLoan(raw json.RawMessage, credentials displayCredentials) (gameapi.Loan, error) {
	clean, err := displayJSON(raw, credentials)
	if err != nil {
		return gameapi.Loan{}, errors.New("redact recorded borrower loan")
	}
	var loan gameapi.Loan
	if err := json.Unmarshal(clean, &loan); err != nil {
		return gameapi.Loan{}, errors.New("decode recorded borrower loan")
	}
	return loan, nil
}

func addLoanPence(total *int64, amount int64) error {
	if amount < 0 || amount > math.MaxInt64-*total {
		return fmt.Errorf("loan total amount: %w", ErrInvalidInput)
	}
	*total += amount
	return nil
}

func recordedLoanLenders(tx *gorm.DB, lenders map[string]bool) (map[string]string, error) {
	names := make(map[string]string, len(lenders))
	if len(lenders) == 0 {
		return names, nil
	}
	ids := make([]string, 0, len(lenders))
	for id := range lenders {
		ids = append(ids, id)
	}
	var rows []struct {
		CompanyID string
		Name      string
	}
	// Reject oversized names rather than truncating before credential redaction:
	// a truncated secret would no longer match its complete original bytes.
	if err := tx.Raw(`SELECT DISTINCT ON (company_id) company_id, name FROM (
		SELECT company_id, CASE WHEN octet_length(name) <= ? THEN name ELSE '' END AS name, observed_at
		FROM configured_company_identities WHERE company_id IN ?
		UNION ALL
		SELECT company_id, CASE WHEN octet_length(company_name) <= ? THEN company_name ELSE '' END AS name, observed_at
		FROM rival_companies WHERE company_id IN ? AND company_name <> ''
	) names ORDER BY company_id, observed_at DESC, name`,
		maxLoanLenderNameBytes, ids, maxLoanLenderNameBytes, ids).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		names[row.CompanyID] = row.Name
	}
	return names, nil
}
