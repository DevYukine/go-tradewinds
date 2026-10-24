package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

func underwritingPositionFixture(t *testing.T, db *Store, account, id string) UnderwritingPosition {
	t.Helper()
	ctx := context.Background()
	id = uuid.NewSHA1(uuid.NameSpaceURL, []byte(account+":"+id)).String()
	policy := json.RawMessage(`{"id":"policy-one","insured_company_id":"foreign-company","ship_id":"foreign-ship","origin_port_id":"cadiz","dest_port_id":"lisbon","insured_value_pence":600000,"hull_value_pence":600000,"premium_bps":3000,"total_premium_pence":180000,"covered_bps":500,"status":"active"}`)
	position := UnderwritingPosition{ID: id, AccountID: account, ActionID: id + "-action", PolicyID: "policy-one", CompanyID: "company-" + account, SliceBPS: 500, CollateralPence: 30000, PremiumPence: 9000, FrozenPolicy: policy, AcceptedAt: time.Now().UTC().Truncate(time.Microsecond), Status: "active"}
	if _, err := db.BeginAction(ctx, &Action{ID: position.ActionID, AccountID: account, Method: http.MethodPost, Path: "/v1/insurance/underwrite", Body: json.RawMessage(`{"policy_id":"policy-one","slice_bps":500}`)}); err != nil {
		t.Fatal(err)
	}
	response, err := json.Marshal(map[string]any{"policy": policy, "underwritings": []any{map[string]any{"id": id, "policy_id": position.PolicyID, "underwriter_company_id": position.CompanyID, "slice_bps": position.SliceBPS, "collateral_locked_pence": position.CollateralPence, "premium_share_pence": position.PremiumPence}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteAction(ctx, position.ActionID, http.StatusOK, response, nil); err != nil {
		t.Fatal(err)
	}
	return position
}

func TestUnderwritingRiskSurvivesRestartAndChargesFullOutstandingLoss(t *testing.T) {
	db, dsn, key := integrationStore(t)
	ctx := context.Background()
	const account = "underwriting-risk"
	saveTestAccount(t, db, account)
	if _, err := db.EnsureUnderwritingBudget(ctx, account, 100000); err != nil {
		t.Fatal(err)
	}
	position := underwritingPositionFixture(t, db, account, "slice-loss")
	if err := db.PutUnderwritingPosition(ctx, &position); err != nil {
		t.Fatal(err)
	}
	if err := db.PutUnderwritingPosition(ctx, &position); err != nil {
		t.Fatal(err)
	}
	risk, err := db.UnderwritingRisk(ctx, account)
	if err != nil || risk.OutstandingPence != 30000 || risk.RemainingPence != 70000 || risk.RealizedNetPence != 0 {
		t.Fatalf("collateral must reserve the full loss once, not the estimated claim: %+v %v", risk, err)
	}
	other, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	risk, err = other.EnsureUnderwritingBudget(ctx, account, 900000)
	if err != nil || risk.InitialPence != 100000 || risk.RemainingPence != 70000 {
		t.Fatalf("restart or larger cash reset the pilot budget: %+v %v", risk, err)
	}
	closure := UnderwritingClosure{AccountID: account, ID: position.ID, Status: "claimed_loss", EarnedPremiumPence: 9000, ClaimsPence: 30000, ClosedAt: time.Now().UTC().Truncate(time.Microsecond), Evidence: json.RawMessage(`{"status":"claimed_loss","policy_id":"policy-one"}`)}
	if err := other.CloseUnderwritingPosition(ctx, closure); err != nil {
		t.Fatal(err)
	}
	if err := other.CloseUnderwritingPosition(ctx, closure); err != nil {
		t.Fatal(err)
	}
	risk, err = other.UnderwritingRisk(ctx, account)
	if err != nil || risk.OutstandingPence != 0 || risk.RealizedNetPence != -21000 || risk.RealizedLossPence != 21000 || risk.RemainingPence != 79000 {
		t.Fatalf("earned premium and full claim must settle once without principal income: %+v %v", risk, err)
	}
	closure.ReturnedCollateralPence = 30000
	if err := other.CloseUnderwritingPosition(ctx, closure); err == nil {
		t.Fatal("conflicting collateral refund rewrote the accepted loss")
	}
	changed := position
	changed.PremiumPence++
	if err := db.PutUnderwritingPosition(ctx, &changed); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("changed receipt identity/value was accepted: %v", err)
	}
	positions, err := db.UnderwritingPositions(ctx, account)
	if err != nil || len(positions) != 1 || positions[0].NetPence == nil || *positions[0].NetPence != -21000 {
		t.Fatalf("durable owned position lost settled accounting: %+v %v", positions, err)
	}
}

func TestUnderwritingWinningPrincipalIsNotProfitOrFreshPilotCapital(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const account = "underwriting-win"
	saveTestAccount(t, db, account)
	if _, err := db.EnsureUnderwritingBudget(ctx, account, 100000); err != nil {
		t.Fatal(err)
	}
	position := underwritingPositionFixture(t, db, account, "slice-safe")
	if err := db.PutUnderwritingPosition(ctx, &position); err != nil {
		t.Fatal(err)
	}
	if err := db.CloseUnderwritingPosition(ctx, UnderwritingClosure{AccountID: account, ID: position.ID, Status: "settled_safe", EarnedPremiumPence: 9000, ReturnedCollateralPence: 30000, ClosedAt: time.Now().UTC(), Evidence: json.RawMessage(`{"status":"settled_safe","policy_id":"policy-one"}`)}); err != nil {
		t.Fatal(err)
	}
	risk, err := db.UnderwritingRisk(ctx, account)
	if err != nil || risk.RealizedNetPence != 9000 || risk.RemainingPence != 100900 || risk.OutstandingPence != 0 {
		t.Fatalf("only 10%% of actual net, never collateral principal, replenishes pilot capacity: %+v %v", risk, err)
	}
}
