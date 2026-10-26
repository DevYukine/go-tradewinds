package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func (r *Runner) loanCaptureTask(ctx context.Context) (fetchTask, error) {
	task := fetchTask{key: "loans", kind: fetchBorrowerLoans}
	pending, err := r.db.PendingActions(ctx, r.account.ID)
	if err != nil {
		return task, err
	}
	for _, action := range pending {
		if strings.HasPrefix(action.Path, "/v1/loan/") {
			return fetchTask{key: "loans", path: "/v1/loan/list?role=borrower", kind: fetchPages}, nil
		}
	}
	for _, shipID := range []string{"__growth", "__treasury", "__loans"} {
		mission, data, err := r.loadMission(ctx, shipID)
		if err != nil {
			return task, err
		}
		if mission != nil && (data.LoanRepayment != nil ||
			data.Capacity != nil && data.Capacity.Financing != nil ||
			data.Action != nil && strings.HasPrefix(data.Action.Path, "/v1/loan/")) {
			return fetchTask{key: "loans", path: "/v1/loan/list?role=borrower", kind: fetchPages}, nil
		}
	}
	return task, nil
}

// validateBorrowerLoans distinguishes a complete empty loan book from missing
// debt authority. Scalar zero values alone cannot distinguish null or omitted
// fields from an observed zero balance.
func validateBorrowerLoans(raw json.RawMessage, companyID string) error {
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return fmt.Errorf("invalid borrower loan list: %w", err)
	}
	if rows == nil {
		return errors.New("borrower loan list is null")
	}
	seen := make(map[string]bool, len(rows))
	for index, row := range rows {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(row, &fields); err != nil {
			return fmt.Errorf("invalid borrower loan row %d: %w", index, err)
		}
		for _, key := range []string{
			"id", "borrower_company_id", "banking_region", "status",
			"owed_pence", "arrears_pence", "collateral_kind", "collateral_status",
		} {
			if value := bytes.TrimSpace(fields[key]); len(value) == 0 || bytes.Equal(value, []byte("null")) {
				return fmt.Errorf("borrower loan row %d has unknown %s", index, key)
			}
		}
		var loan gameapi.Loan
		if err := json.Unmarshal(row, &loan); err != nil {
			return fmt.Errorf("invalid borrower loan row %d: %w", index, err)
		}
		if strings.TrimSpace(loan.ID) == "" || companyID == "" ||
			loan.BorrowerCompanyID != companyID || seen[loan.ID] {
			return fmt.Errorf("borrower loan row %d has invalid or duplicate identity", index)
		}
		seen[loan.ID] = true
		switch loan.BankingRegion {
		case "channel", "north_sea", "baltic", "iberia", "caribbean", "indian_ocean", "east_indies", "america":
		default:
			return fmt.Errorf("borrower loan row %d has unknown banking region", index)
		}
		switch loan.Status {
		case "offered", "active", "repaid", "defaulted", "recovered", "written_off", "withdrawn", "declined", "lapsed":
		default:
			return fmt.Errorf("borrower loan row %d has unknown status", index)
		}
		switch loan.CollateralKind {
		case "ship_lien", "warehouse_goods", "contract_receivable", "unsecured":
		default:
			return fmt.Errorf("borrower loan row %d has unknown collateral kind", index)
		}
		switch loan.CollateralStatus {
		case "none", "pledged", "held", "released", "seizure_pending", "seized", "lost":
		default:
			return fmt.Errorf("borrower loan row %d has unknown collateral status", index)
		}
		if loan.OwedPence < 0 || loan.ArrearsPence < 0 {
			return fmt.Errorf("borrower loan row %d has negative debt obligations", index)
		}
		for _, amount := range []*int64{loan.InstallmentPence, loan.NextPaymentPence} {
			if amount != nil && *amount < 0 {
				return fmt.Errorf("borrower loan row %d has negative payment terms", index)
			}
		}
		if (loan.Status == "active" || loan.Status == "defaulted") && loan.OwedPence > 0 {
			for _, key := range []string{"outstanding_principal_pence", "rate_basis"} {
				if value := bytes.TrimSpace(fields[key]); len(value) == 0 || bytes.Equal(value, []byte("null")) {
					return fmt.Errorf("borrower loan row %d has unknown %s", index, key)
				}
			}
			if loan.OutstandingPrincipalPence < 0 {
				return fmt.Errorf("borrower loan row %d has negative outstanding principal", index)
			}
			switch loan.RateBasis {
			case "annual":
				if loan.AnnualRateBPS == nil || *loan.AnnualRateBPS < 0 {
					return fmt.Errorf("borrower loan row %d has unknown or invalid annual interest", index)
				}
			case "daily":
				value := bytes.TrimSpace(fields["daily_rate_bps"])
				if len(value) == 0 || bytes.Equal(value, []byte("null")) || loan.DailyRateBPS < 0 {
					return fmt.Errorf("borrower loan row %d has unknown or invalid daily interest", index)
				}
			default:
				return fmt.Errorf("borrower loan row %d has unknown interest basis", index)
			}
		}
		if loan.CollateralKind == "ship_lien" &&
			(loan.CollateralStatus == "pledged" || loan.CollateralStatus == "held" || loan.CollateralStatus == "seizure_pending") &&
			(loan.CollateralShipID == nil || strings.TrimSpace(*loan.CollateralShipID) == "") {
			return fmt.Errorf("borrower loan row %d has unknown pledged ship identity", index)
		}
		if loan.Status == "active" {
			if loan.Repayment != "amortised" && loan.Repayment != "balloon" ||
				loan.NextPaymentPence == nil || loan.Repayment == "amortised" && loan.InstallmentPence == nil {
				return fmt.Errorf("borrower loan row %d has unknown active payment obligations", index)
			}
		}
	}
	return nil
}
