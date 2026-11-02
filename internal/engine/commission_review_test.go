package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func acceptedCommission(t *testing.T) (*recoveryHarness, *Runner, *store.Mission, missionData) {
	t.Helper()
	h, runner := commissionHarness(t)
	loanCapacityRefresh(t, h, runner)
	if changed, err := runner.driveCommissions(h.ctx); !changed || err != nil {
		t.Fatalf("admit: changed=%v err=%v", changed, err)
	}
	mission, data := h.mission(commissionMission)
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil || !data.Commission.Accepted {
		t.Fatalf("accept: accepted=%v err=%v", data.Commission.Accepted, err)
	}
	loanCapacityRefresh(t, h, runner)
	return h, runner, mission, data
}

func TestCommissionAdmissionWaitsForFlagshipAndResidentFactor(t *testing.T) {
	for _, reason := range []string{"flagship", "flagship loan", "flagship retry", "ship presence only"} {
		t.Run(reason, func(t *testing.T) {
			h, runner := commissionHarness(t)
			loanCapacityRefresh(t, h, runner)
			switch reason {
			case "flagship":
				runner.snapshot["_america_flagship"] = encode(americaFlagshipState{Pending: true})
			case "flagship loan":
				runner.snapshot["_america_flagship"] = encode(americaFlagshipState{LoanID: "funded-loan"})
			case "flagship retry":
				runner.snapshot["_america_flagship"] = encode(americaFlagshipState{RetryAt: time.Now().Add(time.Minute)})
			default:
				runner.stateCache.Factors = nil
				ship := &runner.stateCache.Ships[0]
				ship.Ship.CurrentPortID, ship.Ship.Status, ship.Voyage = "boston", "berthed", nil
			}
			if changed, err := runner.driveCommissions(h.ctx); changed || err != nil {
				t.Fatalf("commission bypassed %s: changed=%v err=%v", reason, changed, err)
			}
			h.missingMission(commissionMission)
		})
	}
}

func TestCommissionAdmissionUsesSnapshotAndOneFullyFundedRemit(t *testing.T) {
	h, runner := commissionHarness(t)
	h.api.setVaultBalance("america", 0)
	loanCapacityRefresh(t, h, runner)
	runner.stateCache.Rates = append(runner.stateCache.Rates, Rate{FromRegion: "north", ToRegion: "america", FeeBPS: 100})
	base := h.api.server.Config.Handler
	quoteGets := 0
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/shipyard/quote" {
			quoteGets++
		}
		base.ServeHTTP(w, req)
	})
	if changed, err := runner.driveCommissions(h.ctx); !changed || err != nil {
		t.Fatalf("remittable build/deposit were refused: changed=%v err=%v", changed, err)
	}
	mission, data := h.mission(commissionMission)
	if quoteGets != 0 || data.Action == nil || data.Action.Stage != "remit" || data.Commission.Decision.FundingFeePence <= 0 || data.BuildName != "" {
		t.Fatalf("admission fetched an extra quote, omitted fees or consumed a name: gets=%d data=%+v", quoteGets, data)
	}
	var request struct {
		Gross int64 `json:"gross_amount_pence"`
	}
	if err := json.Unmarshal(data.Action.Body, &request); err != nil {
		t.Fatal(err)
	}
	fee := request.Gross / 100
	if _, err := runner.consumeResult(h.ctx, mission, &data, http.StatusOK, encode(map[string]int64{
		"gross_amount_pence": request.Gross, "fee_pence": fee, "net_credited_pence": request.Gross - fee,
	})); err != nil {
		t.Fatal(err)
	}
	// Even stale balances or a growing reservation cannot buy a second remit.
	if changed, err := runner.driveCommissions(h.ctx); changed || err != nil {
		t.Fatalf("unchanged shortfall staged a second fee: changed=%v err=%v", changed, err)
	}
	mission, data = h.mission(commissionMission)
	if data.Action != nil {
		t.Fatal("single funding remit repeated")
	}
	h.api.setVaultBalance("america", request.Gross-fee)
	h.api.setVaultBalance("north", 50_000_000-request.Gross)
	loanCapacityRefresh(t, h, runner)
	if changed, err := runner.driveCommissions(h.ctx); !changed || err != nil {
		t.Fatalf("funded order did not advance: changed=%v err=%v", changed, err)
	}
	mission, data = h.mission(commissionMission)
	if data.Action == nil || data.Action.Stage != "commission-accept" || data.FeesPence != fee {
		t.Fatalf("funded commission lost fee or acceptance: %+v", data)
	}
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil || !data.Commission.Accepted || data.Commission.FundingShortfallPence != 0 {
		t.Fatalf("acceptance did not renew delivery funding allowance: %+v %v", data.Commission, err)
	}
	h.api.setVaultBalance("america", 0)
	loanCapacityRefresh(t, h, runner)
	runner.stateCache.Rates = append(runner.stateCache.Rates, Rate{FromRegion: "north", ToRegion: "america", FeeBPS: 100})
	if changed, err := runner.driveCommissions(h.ctx); !changed || err != nil {
		t.Fatalf("accepted delivery could not fund its new shortfall: %v %v", changed, err)
	}
	_, data = h.mission(commissionMission)
	if data.Action == nil || data.Action.Stage != "remit" {
		t.Fatal("pre-acceptance funding blocked delivery funding")
	}
}

func TestCommissionFundingFeeMustClearStrictMargin(t *testing.T) {
	now := time.Now().UTC()
	row, port, quote := commissionTestTerms(now)
	base := evaluateCommission(row, port, quote, 2, now, 60, 30_000_000, 0)
	fee := base.NetPence - base.MarginPence
	if got := evaluateCommission(row, port, quote, 2, now, 60, 30_000_000, fee); got.Accepted || got.NetPence != got.MarginPence {
		t.Fatalf("funding fee was not charged against strict margin: %+v", got)
	}
}

func TestCommissionDeliveryUsesRawCashAndRecoverableDeposit(t *testing.T) {
	h, runner, _, data := acceptedCommission(t)
	// America's other reservations can grow after acceptance. Delivery returns
	// the cash atomically instead of consuming it like a fleet hull purchase.
	runner.snapshot["_america_flagship"] = encode(americaFlagshipState{Pending: true})
	quote := runner.stateCache.Quotes["boston|"+recoveryHull]
	for index := range runner.stateCache.Vaults {
		if runner.stateCache.Vaults[index].BankingRegion == "america" {
			runner.stateCache.Vaults[index].BalancePence = *quote.TotalCostPence
		}
	}
	if changed, err := runner.driveCommissions(h.ctx); !changed || err != nil {
		t.Fatalf("funded loss-mitigating delivery held: changed=%v err=%v", changed, err)
	}
	_, data = h.mission(commissionMission)
	if data.Action == nil || data.Action.Stage != "commission-deliver" {
		t.Fatal("other reservations caused a pointless remit instead of delivery")
	}
	if err := runner.commissionPreflight(h.ctx, &data); err != nil {
		t.Fatalf("raw cash exactly at quote failed: %v", err)
	}
	for index := range runner.stateCache.Vaults {
		if runner.stateCache.Vaults[index].BankingRegion == "america" {
			runner.stateCache.Vaults[index].BalancePence--
		}
	}
	if err := runner.commissionPreflight(h.ctx, &data); err == nil {
		t.Fatal("delivery spent a penny beyond raw cash")
	}
	base := h.api.server.Config.Handler
	ceiling := data.Commission.Contract.RewardPence + data.Commission.Contract.CollateralPence
	liveCost := ceiling
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/shipyard/quote" {
			quote.TotalCostPence = &liveCost
			h.api.writeJSON(w, http.StatusOK, quote)
			return
		}
		base.ServeHTTP(w, req)
	})
	for index := range runner.stateCache.Vaults {
		if runner.stateCache.Vaults[index].BankingRegion == "america" {
			runner.stateCache.Vaults[index].BalancePence = ceiling + 1
		}
	}
	if err := runner.commissionPreflight(h.ctx, &data); err != nil {
		t.Fatalf("delivery at reward plus recoverable deposit was refused: %v", err)
	}
	liveCost++
	if err := runner.commissionPreflight(h.ctx, &data); err == nil {
		t.Fatal("delivery spent more than reward plus recoverable deposit")
	}
}

func TestCommissionPendingDeliveryReplaysReceiptBeforeTerminalCloseout(t *testing.T) {
	h, runner, _, _ := acceptedCommission(t)
	if _, err := runner.driveCommissions(h.ctx); err != nil {
		t.Fatal(err)
	}
	mission, data := h.mission(commissionMission)
	action := data.Action
	if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
		t.Fatal(err)
	}
	// The server commits but the journal never receives the first reply.
	var lost json.RawMessage
	if err := runner.api.Do(h.ctx, action.Method, action.Path, json.RawMessage(action.Body), action.ID, &lost); err != nil {
		t.Fatal(err)
	}
	loanCapacityRefresh(t, h, runner)
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if data.Action != nil || data.Settlement == nil || !data.Settlement.CostsKnown || data.Settlement.AcquisitionPence != 20_570_000 {
		t.Fatalf("completed listing stole exact replay receipt: %+v", data)
	}
	if _, err := runner.driveCommissions(h.ctx); err != nil {
		t.Fatal(err)
	}
	history, err := h.db.GrowthHistory(h.ctx, h.account.ID, "", time.Now().UTC())
	if err != nil || history.UnknownCosts || history.NetPence != 0 {
		t.Fatalf("commission contaminated hull evidence: %+v %v", history, err)
	}
}

func TestCommissionMissingBuilderExpiresAndBooksDeposit(t *testing.T) {
	h, runner, mission, data := acceptedCommission(t)
	data.Commission.Contract.ExpiresAt = new(time.Now().UTC().Add(-time.Hour))
	if err := runner.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	runner.snapshot["contracts"] = encode([]gameapi.CommissionListing{})
	if changed, err := runner.driveCommissions(h.ctx); !changed || err != nil {
		t.Fatalf("expired missing builder held its cash forever: changed=%v err=%v", changed, err)
	}
	_, closed := h.mission(commissionMission)
	if closed.Settlement == nil || !closed.Settlement.CostsKnown || closed.Settlement.ExpensesPence != 2_600_000 || closed.Settlement.ProceedsPence != 0 {
		t.Fatalf("missing-builder expiry did not book actual deposit risk: %+v", closed)
	}
}

func TestCommissionPresenceRefusalDoesNotBurnNameOrBlacklistUntilExpiry(t *testing.T) {
	h, runner := commissionHarness(t)
	loanCapacityRefresh(t, h, runner)
	used := string(runner.snapshot["_used_ship_names"])
	if _, err := runner.driveCommissions(h.ctx); err != nil {
		t.Fatal(err)
	}
	mission, data := h.mission(commissionMission)
	if _, err := runner.consumeCommissionResult(h.ctx, mission, &data, http.StatusForbidden, encode(map[string]string{"code": "no_port_presence"})); err != nil {
		t.Fatal(err)
	}
	until := decode[map[string]time.Time](runner.snapshot, "_commission_refusals")[data.Commission.Contract.ID]
	_, upper, _ := runner.serverBounds()
	delay := 15 * time.Minute / time.Duration(max(int64(1), runner.stateCache.Clock.Calendar.TimeScale))
	if !until.After(upper) || until.After(upper.Add(delay+time.Second)) || string(runner.snapshot["_used_ship_names"]) != used {
		t.Fatalf("transient presence failure lost a name or order: cooldown=%v used=%s", until.Sub(upper), runner.snapshot["_used_ship_names"])
	}
}

func TestAmericaFlagshipProtectsOtherCommissionReservations(t *testing.T) {
	h, runner, _, data := acceptedCommission(t)
	runner.snapshot["_america_flagship"] = encode(americaFlagshipState{Pending: true})
	flagship := &missionData{BuildClass: recoveryHull}
	h.api.setVaultBalance("america", 20_670_000)
	loanCapacityRefresh(t, h, runner)
	if _, err := runner.americaFlagshipBuildQuote(h.ctx, flagship); err == nil {
		t.Fatal("flagship spent the accepted commission's build reservation")
	}
	h.api.setVaultBalance("america", data.Commission.BuildLimitPence+20_670_000)
	loanCapacityRefresh(t, h, runner)
	if _, err := runner.americaFlagshipBuildQuote(h.ctx, flagship); err != nil {
		t.Fatalf("flagship counted its own hold-all reservation: %v", err)
	}
}

func TestCommissionRemitRefusalDoesNotWaitOneGameDay(t *testing.T) {
	h, runner := commissionHarness(t)
	h.api.setVaultBalance("america", 0)
	loanCapacityRefresh(t, h, runner)
	runner.stateCache.Rates = append(runner.stateCache.Rates, Rate{FromRegion: "north", ToRegion: "america", FeeBPS: 100})
	if _, err := runner.driveCommissions(h.ctx); err != nil {
		t.Fatal(err)
	}
	mission, data := h.mission(commissionMission)
	if data.Action == nil || data.Action.Stage != "remit" {
		t.Fatal("fixture did not stage commission funding")
	}
	now := time.Now().UTC()
	if _, err := runner.consumeResult(h.ctx, mission, &data, http.StatusBadRequest, encode(map[string]string{"code": "insufficient_funds"})); err != nil {
		t.Fatal(err)
	}
	if data.Action != nil || data.CooldownUntil.After(now.Add(6*time.Second)) {
		t.Fatalf("commission funding refusal delayed its deadline: %+v", data)
	}
}

func TestCommissionExpiredPendingDeliveryWithoutBuilderCloses(t *testing.T) {
	h, runner, _, _ := acceptedCommission(t)
	if _, err := runner.driveCommissions(h.ctx); err != nil {
		t.Fatal(err)
	}
	mission, data := h.mission(commissionMission)
	persisted, err := h.db.BeginAction(h.ctx, &store.Action{ID: data.Action.ID, AccountID: h.account.ID,
		Method: data.Action.Method, Path: data.Action.Path, Body: data.Action.Body})
	if err != nil {
		t.Fatal(err)
	}
	persisted.CreatedAt = time.Now().UTC().Add(-store.ActionReplayWindow)
	data.Commission.Contract.ExpiresAt = new(time.Now().UTC().Add(-time.Hour))
	runner.snapshot["contracts"] = encode([]gameapi.CommissionListing{})
	before := copyCommissionMissionData(t, data)
	if changed, err := runner.reconcileCommission(h.ctx, mission, &data, persisted); !changed || err != nil {
		t.Fatalf("expired missing builder retained a pending delivery: changed=%v err=%v", changed, err)
	}
	if data.Action != nil || data.Settlement == nil || !data.Settlement.CostsKnown || data.Settlement.ExpensesPence != 2_600_000 {
		t.Fatalf("expiry failed to close with the deposit loss: %+v", data)
	}
	// A restart before the mission save uses the same durable expiry boundary.
	reconciled, err := h.db.BeginAction(h.ctx, persisted)
	if err != nil {
		t.Fatal(err)
	}
	completedAt := data.Settlement.CompletedAt
	if _, err := runner.reconcileCommission(h.ctx, mission, &before, reconciled); err != nil ||
		before.Settlement == nil || !before.Settlement.CompletedAt.Equal(completedAt) {
		t.Fatalf("expiry replay changed the economic boundary: %+v %v", before, err)
	}
}

func TestCommissionReplayRetainsCashAndRefusalInputs(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := replayRecorderRunner(t, h)
	t.Cleanup(func() { runner.stopReplay(context.Background()) })
	at := time.Now().UTC()
	replayRecorderSeed(t, h, runner, at)
	runner.snapshot["_america_flagship"] = encode(americaFlagshipState{Pending: true})
	runner.snapshot["_commission_refusals"] = encode(map[string]time.Time{"order": at.Add(time.Minute)})
	runner.flushReplay(h.ctx, at)
	replayRecorderWait(t, func() bool { return len(replayRecorderFrames(t, h)) == 1 })
	frame := replayRecorderFrames(t, h)[0]
	for _, key := range []string{"_america_flagship", "_commission_refusals"} {
		if !bytes.Equal(frame.Datasets[key].Data, runner.snapshot[key]) {
			t.Fatalf("replay omitted commission planning input %s", key)
		}
	}
}

func TestCommissionDoesNotReserveAnEmptyTradePlan(t *testing.T) {
	runner := &Runner{}
	evidence := growthEvidence{}
	mission := &store.Mission{ShipID: commissionMission}
	if !runner.reserveGrowthMission(&evidence, mission, &missionData{Commission: &commissionIntent{}}) ||
		len(evidence.incumbents) != 0 || len(evidence.markets) != 0 {
		t.Fatalf("commission pseudo-mission reserved nonexistent trade depth: %+v", evidence)
	}
}

func TestCommissionFundingRetriesOnlyUnpaidRemits(t *testing.T) {
	for _, outcome := range []string{"financial veto", "refused"} {
		t.Run(outcome, func(t *testing.T) {
			h, runner := commissionHarness(t)
			h.api.setVaultBalance("america", 0)
			loanCapacityRefresh(t, h, runner)
			runner.stateCache.Rates = append(runner.stateCache.Rates, Rate{FromRegion: "north", ToRegion: "america", FeeBPS: 100})
			if _, err := runner.driveCommissions(h.ctx); err != nil {
				t.Fatal(err)
			}
			mission, data := h.mission(commissionMission)
			if data.Action == nil || data.Action.Stage != "remit" || data.Commission.FundingShortfallPence != 0 {
				t.Fatal("staging funding consumed the paid-remit allowance")
			}
			firstID := data.Action.ID
			if outcome == "financial veto" {
				data.Action.FinancialEpoch++
				if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
					t.Fatal(err)
				}
			} else if _, err := runner.consumeResult(h.ctx, mission, &data, http.StatusBadRequest, encode(map[string]string{"code": "insufficient_funds"})); err != nil {
				t.Fatal(err)
			}
			h.expireCooldown(commissionMission)
			if changed, err := runner.driveCommissions(h.ctx); !changed || err != nil {
				t.Fatalf("unpaid remit could not be restaged: %v %v", changed, err)
			}
			_, data = h.mission(commissionMission)
			if data.Action == nil || data.Action.Stage != "remit" || data.Action.ID == firstID || data.Commission.FundingShortfallPence != 0 || data.FeesPence != 0 {
				t.Fatalf("unpaid funding was counted as a successful remit: %+v", data)
			}
		})
	}
}

func TestCommissionFailedReadmissionReleasesCapitalAndBooksPaidFee(t *testing.T) {
	for _, reason := range []string{"quote rose", "margin lost", "flagship retry"} {
		t.Run(reason, func(t *testing.T) {
			h, runner := commissionHarness(t)
			h.api.setVaultBalance("america", 0)
			loanCapacityRefresh(t, h, runner)
			runner.stateCache.Rates = append(runner.stateCache.Rates, Rate{FromRegion: "north", ToRegion: "america", FeeBPS: 100})
			if _, err := runner.driveCommissions(h.ctx); err != nil {
				t.Fatal(err)
			}
			mission, data := h.mission(commissionMission)
			var request struct {
				Gross int64 `json:"gross_amount_pence"`
			}
			if data.Action == nil || json.Unmarshal(data.Action.Body, &request) != nil {
				t.Fatal("fixture did not stage funding")
			}
			fee := request.Gross / 100
			receipt := encode(map[string]int64{
				"gross_amount_pence": request.Gross, "fee_pence": fee, "net_credited_pence": request.Gross - fee,
			})
			action := data.Action
			if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID,
				Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
				t.Fatal(err)
			}
			if err := h.db.CompleteAction(h.ctx, action.ID, http.StatusOK, receipt, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := runner.consumeResult(h.ctx, mission, &data, http.StatusOK, receipt); err != nil {
				t.Fatal(err)
			}
			h.api.setVaultBalance("america", request.Gross-fee)
			loanCapacityRefresh(t, h, runner)
			switch reason {
			case "quote rose":
				quote := runner.stateCache.Quotes["boston|"+recoveryHull]
				quote.TotalCostPence = new(*quote.TotalCostPence + 1)
				runner.stateCache.Quotes["boston|"+recoveryHull] = quote
			case "margin lost":
				rows, err := runner.commissionListings()
				if err != nil {
					t.Fatal(err)
				}
				rows[0].RewardPence = data.Commission.BuildLimitPence
				runner.snapshot["contracts"] = encode(rows)
			case "flagship retry":
				runner.snapshot["_america_flagship"] = encode(americaFlagshipState{RetryAt: time.Now().Add(time.Minute)})
			}
			if changed, err := runner.driveCommissions(h.ctx); !changed || err != nil {
				t.Fatalf("failed readmission held capital: %v %v", changed, err)
			}
			_, data = h.mission(commissionMission)
			if data.Settlement == nil || data.Settlement.FeesPence != fee || data.Settlement.ExpensesPence != 0 || data.Action != nil {
				t.Fatalf("retirement lost paid fee or invented deposit forfeiture: %+v", data)
			}
			if changed, err := runner.driveCommissions(h.ctx); !changed || err != nil {
				t.Fatalf("fee closeout failed: %v %v", changed, err)
			}
			reserved, err := runner.committedPlanningInput(h.ctx, ShipState{})
			if err != nil || reserved.ReserveByRegion["america"] >= data.Commission.BuildLimitPence {
				t.Fatalf("retired commission still reserves build capital: %+v %v", reserved.ReserveByRegion, err)
			}
		})
	}
}
