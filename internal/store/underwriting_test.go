package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestUnderwritingReadsDoNotSeedAnAbsentBudget(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const account = "underwriting-unseeded"
	saveTestAccount(t, db, account)
	risk, err := db.UnderwritingRisk(ctx, account)
	if err != nil || risk != (UnderwritingRisk{}) {
		t.Fatalf("absent pilot budget did not return observed zero risk: %+v %v", risk, err)
	}
	positions, err := db.UnderwritingPositions(ctx, account)
	if err != nil || positions == nil || len(positions) != 0 {
		t.Fatalf("absent positions must be an empty JSON array: %+v %v", positions, err)
	}
	policies, err := db.UnderwritingPolicies(ctx, account)
	if err != nil || policies == nil || len(policies) != 0 {
		t.Fatalf("absent policies must be an empty JSON array: %+v %v", policies, err)
	}
	var seeds int64
	if err := db.db.Raw("SELECT COUNT(*) FROM underwriting_budgets WHERE account_id = ?", account).Scan(&seeds).Error; err != nil {
		t.Fatal(err)
	}
	if seeds != 0 {
		t.Fatal("read-only refresh silently seeded the pilot budget")
	}
	risk, err = db.EnsureUnderwritingBudget(ctx, account, 100000)
	if err != nil || risk.InitialPence != 100000 || risk.RemainingPence != 100000 {
		t.Fatalf("earlier refresh prevented the first explicit pilot seed: %+v %v", risk, err)
	}
}

func TestUnderwritingPolicyEvidenceKeepsFirstTermsAndTerminalFate(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const account = "underwriting-policy-history"
	credentials := saveTestAccount(t, db, account)
	observed := time.Now().UTC().Add(-30 * 24 * time.Hour).Truncate(time.Microsecond)
	observation := func(status, hash string, value int64, source time.Time) UnderwritingPolicyRecord {
		t.Helper()
		payload, err := json.Marshal(map[string]any{
			"id": "policy-history", "insured_company_id": "foreign-company", "ship_id": "foreign-ship",
			"origin_port_id": "cadiz", "dest_port_id": "lisbon", "insured_value_pence": value,
			"hull_value_pence": 600000, "premium_bps": 3000, "total_premium_pence": 180000,
			"covered_bps": 500, "status": status, "api_key": credentials.APIKey,
		})
		if err != nil {
			t.Fatal(err)
		}
		return UnderwritingPolicyRecord{
			AccountID: account, PolicyID: "policy-history", TermsHash: hash, Status: status,
			Payload: payload, SourceUpdatedAt: source, ObservedAt: source,
		}
	}
	put := func(record UnderwritingPolicyRecord) {
		t.Helper()
		if err := db.PutUnderwritingPolicies(ctx, account, []UnderwritingPolicyRecord{record}); err != nil {
			t.Fatal(err)
		}
	}
	read := func() UnderwritingPolicyRecord {
		t.Helper()
		rows, err := db.UnderwritingPolicies(ctx, account)
		if err != nil || len(rows) != 1 {
			t.Fatalf("policy observation missing: %+v %v", rows, err)
		}
		if bytes.Contains(rows[0].Payload, []byte(credentials.APIKey)) || bytes.Contains(rows[0].Payload, []byte("api_key")) {
			t.Fatal("non-public secret fields entered permanent policy evidence")
		}
		return rows[0]
	}
	first := observation("active", "first-terms", 600000, observed)
	put(first)
	put(first)
	terminal := observation("settled_safe", "first-terms", 600000, observed.Add(time.Hour))
	put(terminal)
	stale := first
	stale.ObservedAt = observed.Add(2 * time.Hour)
	put(stale)
	row := read()
	if row.Status != "settled_safe" || row.TermsChanged || !row.SourceUpdatedAt.Equal(terminal.SourceUpdatedAt) {
		t.Fatalf("an older open observation rolled back/censored the later terminal policy: %+v", row)
	}
	put(observation("claimed_loss", "first-terms", 600000, observed.Add(3*time.Hour)))
	row = read()
	if row.Status != "claimed_loss" || !row.TermsChanged || row.TermsHash != "first-terms" {
		t.Fatalf("latest contradictory terminal fate was lost or not censored: %+v", row)
	}
	put(observation("settled_safe", "changed-terms", 500000, observed.Add(4*time.Hour)))
	put(observation("settled_safe", "first-terms", 600000, observed.Add(5*time.Hour)))
	if err := db.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	row = read()
	if row.TermsHash != "first-terms" || !row.TermsChanged || !row.SourceUpdatedAt.Equal(observed.Add(5*time.Hour)) {
		t.Fatalf("term reversal or raw retention erased permanent censorship: %+v", row)
	}
	foreign := first
	foreign.AccountID = "another-account"
	if err := db.PutUnderwritingPolicies(ctx, account, []UnderwritingPolicyRecord{foreign}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("foreign policy observation was accepted: %v", err)
	}
}

func TestUnderwritingPolicyTermsDriftCannotBeClearedByReverting(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const account = "underwriting-terms-drift"
	saveTestAccount(t, db, account)
	position := underwritingPositionFixture(t, db, account, "terms-template")
	observed := time.Now().UTC().Truncate(time.Microsecond)
	record := UnderwritingPolicyRecord{
		AccountID: account, PolicyID: position.PolicyID, TermsHash: "first-fingerprint", Status: "active",
		Payload: position.FrozenPolicy, SourceUpdatedAt: observed, ObservedAt: observed,
	}
	if err := db.PutUnderwritingPolicies(ctx, account, []UnderwritingPolicyRecord{record}); err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(position.FrozenPolicy, &payload); err != nil {
		t.Fatal(err)
	}
	payload["insured_value_pence"] = json.RawMessage("500000")
	changed, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	record.Payload = changed
	record.SourceUpdatedAt = observed.Add(time.Second)
	record.ObservedAt = record.SourceUpdatedAt
	if err := db.PutUnderwritingPolicies(ctx, account, []UnderwritingPolicyRecord{record}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.UnderwritingPolicies(ctx, account)
	if err != nil || len(rows) != 1 || !rows[0].TermsChanged {
		t.Fatalf("reused hash hid an actual change in frozen policy terms: %+v %v", rows, err)
	}
	record.Payload = position.FrozenPolicy
	record.TermsHash = "replacement-fingerprint"
	record.SourceUpdatedAt = observed.Add(2 * time.Second)
	record.ObservedAt = record.SourceUpdatedAt
	if err := db.PutUnderwritingPolicies(ctx, account, []UnderwritingPolicyRecord{record}); err != nil {
		t.Fatal(err)
	}
	rows, err = db.UnderwritingPolicies(ctx, account)
	if err != nil || len(rows) != 1 || !rows[0].TermsChanged || rows[0].TermsHash != "first-fingerprint" {
		t.Fatalf("reverting terms or replacing the hash erased the first fingerprint/censorship: %+v %v", rows, err)
	}
}

func TestUnderwritingPositionRequiresOwnedCompletePresentReceipt(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const account = "underwriting-receipt-owner"
	saveTestAccount(t, db, account)
	saveTestAccount(t, db, "underwriting-receipt-other")
	base := underwritingPositionFixture(t, db, account, "receipt-template")
	cases := []struct {
		name    string
		mutate  func(map[string]any, map[string]any)
		pending bool
		foreign bool
	}{
		{name: "missing-premium", mutate: func(_ map[string]any, receipt map[string]any) { delete(receipt, "premium_share_pence") }},
		{name: "null-collateral", mutate: func(_ map[string]any, receipt map[string]any) { receipt["collateral_locked_pence"] = nil }},
		{name: "changed-slice", mutate: func(_ map[string]any, receipt map[string]any) { receipt["slice_bps"] = 1000 }},
		{name: "wrong-company", mutate: func(_ map[string]any, receipt map[string]any) { receipt["underwriter_company_id"] = "another-company" }},
		{name: "duplicate-id", mutate: func(response map[string]any, receipt map[string]any) {
			response["underwritings"] = []any{receipt, receipt}
		}},
		{name: "pending", pending: true},
		{name: "foreign-action", foreign: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			position := base
			position.ID = uuid.NewSHA1(uuid.NameSpaceURL, []byte(account+":"+test.name)).String()
			position.ActionID = test.name + "-action"
			if _, err := db.BeginAction(ctx, &Action{
				ID: position.ActionID, AccountID: account, Method: http.MethodPost,
				Path: "/v1/insurance/underwrite", Body: json.RawMessage(`{"policy_id":"policy-one","slice_bps":500}`),
			}); err != nil {
				t.Fatal(err)
			}
			receipt := map[string]any{
				"id": position.ID, "policy_id": position.PolicyID, "underwriter_company_id": position.CompanyID,
				"slice_bps": position.SliceBPS, "collateral_locked_pence": position.CollateralPence,
				"premium_share_pence": position.PremiumPence,
			}
			response := map[string]any{"policy": position.FrozenPolicy, "underwritings": []any{receipt}}
			if test.mutate != nil {
				test.mutate(response, receipt)
			}
			if !test.pending {
				body, err := json.Marshal(response)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.CompleteAction(ctx, position.ActionID, http.StatusOK, body, nil); err != nil {
					t.Fatal(err)
				}
			}
			if test.foreign {
				position.AccountID = "underwriting-receipt-other"
				position.CompanyID = "company-underwriting-receipt-other"
			}
			if err := db.PutUnderwritingPosition(ctx, &position); err == nil {
				t.Fatal("unresolved, foreign or incomplete action evidence created a position")
			}
		})
	}
	positions, err := db.UnderwritingPositions(ctx, account)
	if err != nil || len(positions) != 0 {
		t.Fatalf("a rejected receipt left permanent exposure: %+v %v", positions, err)
	}
}

func TestUnderwritingClosureReplayPreservesEvidenceAndCash(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const account = "underwriting-closure-evidence"
	credentials := saveTestAccount(t, db, account)
	position := underwritingPositionFixture(t, db, account, "closure-slice")
	if err := db.PutUnderwritingPosition(ctx, &position); err != nil {
		t.Fatal(err)
	}
	evidence, err := json.Marshal(map[string]any{
		"policy_id": position.PolicyID, "status": "settled_safe", "api_key": credentials.APIKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	closure := UnderwritingClosure{
		AccountID: account, ID: position.ID, Status: "settled_safe", EarnedPremiumPence: 9000,
		ReturnedCollateralPence: 30000, FeesPence: 1000, ClosedAt: time.Now().UTC(), Evidence: evidence,
	}
	if err := db.CloseUnderwritingPosition(ctx, closure); err != nil {
		t.Fatal(err)
	}
	if err := db.CloseUnderwritingPosition(ctx, closure); err != nil {
		t.Fatal(err)
	}
	if err := db.PutUnderwritingPosition(ctx, &position); err != nil {
		t.Fatalf("receipt replay after closure was not idempotent: %v", err)
	}
	changed := closure
	changed.Evidence = json.RawMessage(`{"policy_id":"another-policy","status":"settled_safe"}`)
	if err := db.CloseUnderwritingPosition(ctx, changed); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("different closure evidence replaced the original: %v", err)
	}
	changed = closure
	changed.FeesPence++
	if err := db.CloseUnderwritingPosition(ctx, changed); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("different closure cash replaced the original: %v", err)
	}
	var stored string
	if err := db.db.Raw("SELECT closure_evidence::text FROM underwriting_positions WHERE account_id = ? AND position_lookup = ?", account, db.privateLookup("underwriting-position", account, position.ID)).Scan(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if bytes.Contains([]byte(stored), []byte(credentials.APIKey)) {
		t.Fatal("closure evidence retained plaintext credentials")
	}
	positions, err := db.UnderwritingPositions(ctx, account)
	if err != nil || len(positions) != 1 || positions[0].NetPence == nil || *positions[0].NetPence != 8000 ||
		positions[0].ReturnedCollateralPence == nil || *positions[0].ReturnedCollateralPence != 30000 {
		t.Fatalf("receipt replay or a conflicting closure changed settled cash: %+v %v", positions, err)
	}
}

func TestUnderwritingRiskRejectsUnrepresentableIntegerBudget(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const account = "underwriting-risk-overflow"
	saveTestAccount(t, db, account)
	if _, err := db.EnsureUnderwritingBudget(ctx, account, math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	position := underwritingPositionFixture(t, db, account, "overflow-slice")
	if err := db.PutUnderwritingPosition(ctx, &position); err != nil {
		t.Fatal(err)
	}
	if err := db.CloseUnderwritingPosition(ctx, UnderwritingClosure{
		AccountID: account, ID: position.ID, Status: "settled_safe", EarnedPremiumPence: 9000,
		ReturnedCollateralPence: 30000, ClosedAt: time.Now().UTC(), Evidence: json.RawMessage(`{"status":"settled_safe"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if risk, err := db.UnderwritingRisk(ctx, account); err == nil {
		t.Fatalf("overflow wrapped or rounded the durable risk budget: %+v", risk)
	}
}

func TestUnderwritingFundingProofIsAccountScopedAndImmutable(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const account = "underwriting-funding-proof"
	credentials := saveTestAccount(t, db, account)
	saveTestAccount(t, db, "underwriting-funding-other")
	position := underwritingPositionFixture(t, db, account, "funding-slice")
	position.FundingEvidence = json.RawMessage(`{"unverified":true}`)
	if err := db.PutUnderwritingPosition(ctx, &position); err != nil {
		t.Fatal(err)
	}
	positions, err := db.UnderwritingPositions(ctx, account)
	if err != nil || len(positions) != 1 || positions[0].FundingEvidence != nil {
		t.Fatalf("acceptance invented a funding proof: %+v %v", positions, err)
	}
	evidence, err := json.Marshal(map[string]any{
		"banking_region": "north",
		"entry": map[string]any{
			"id": "funding-ledger", "debit_account_id": "vault-north", "amount_pence": position.CollateralPence,
			"created_at": position.AcceptedAt, "metadata": map[string]any{"underwriting_id": position.ID},
			"api_key": credentials.APIKey,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordUnderwritingFunding(ctx, account, position.ID, evidence); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordUnderwritingFunding(ctx, account, position.ID, evidence); err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(evidence, []byte(`"north"`), []byte(`"south"`), 1)
	if err := db.RecordUnderwritingFunding(ctx, account, position.ID, changed); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("changed funding provenance replaced permanent proof: %v", err)
	}
	if err := db.RecordUnderwritingFunding(ctx, "underwriting-funding-other", position.ID, evidence); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign account recorded funding proof: %v", err)
	}
	for _, invalid := range []json.RawMessage{
		json.RawMessage(`null`), json.RawMessage(`{"entry":{},"banking_region":""}`),
		json.RawMessage(`{"entry":null,"banking_region":"north"}`),
	} {
		if err := db.RecordUnderwritingFunding(ctx, account, position.ID, invalid); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid funding proof was accepted: %s %v", invalid, err)
		}
	}
	position.FundingEvidence = nil
	if err := db.PutUnderwritingPosition(ctx, &position); err != nil {
		t.Fatalf("accepted receipt replay conflicted with later funding proof: %v", err)
	}
	positions, err = db.UnderwritingPositions(ctx, account)
	if err != nil || len(positions) != 1 || positions[0].FundingEvidence == nil ||
		bytes.Contains(positions[0].FundingEvidence, []byte(credentials.APIKey)) ||
		!bytes.Contains(positions[0].FundingEvidence, []byte(`"north"`)) {
		t.Fatalf("funding proof was erased, changed or exposed credentials: %+v %v", positions, err)
	}
}
