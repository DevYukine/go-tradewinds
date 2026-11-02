package engine

import (
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestFuturesNegativeProofWaitsBeyondTheLastTransportAttempt(t *testing.T) {
	for _, recent := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed earlier", true: "recent transport"}[recent], func(t *testing.T) {
			h, _, runner, mission, data := futuresStagedAccept(t)
			seedAgedBulletinFixture(t, h, runner, mission, &data, store.ActionReplayWindow+time.Hour)
			attempt := time.Now().UTC().Add(-time.Hour)
			if recent {
				attempt = time.Now().UTC()
			}
			data.Action.FuturesAttemptedAt = attempt
			before := time.Now().UTC()
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed || data.Action == nil {
				t.Fatalf("negative observation fence not persisted: %t %v %+v", changed, err, data.Action)
			}
			fence := data.Action.FuturesObserveAfter
			if fence.Before(before) || recent && !fence.Equal(attempt.Add(gameapi.RequestTimeout+5*time.Second)) ||
				!recent && fence.After(time.Now().UTC()) {
				t.Fatalf("wrong post-attempt fence: recent=%t attempt=%s fence=%s", recent, attempt, fence)
			}
			meta := runner.meta["futures"]
			meta.Complete, meta.Stale, meta.Error = true, false, ""
			meta.AttemptedAt, meta.UpdatedAt = time.Now().UTC(), time.Now().UTC()
			runner.meta["futures"] = meta
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || changed == recent || (data.Action != nil) != recent {
				t.Fatalf("negative proof ignored transport safety window: recent=%t changed=%t err=%v action=%+v", recent, changed, err, data.Action)
			}
		})
	}
}

func TestFuturesLateCommitAfterNegativeObservationIsReadopted(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "atomic pair"}[batch], func(t *testing.T) {
			staged := futuresStagedAccept
			if batch {
				staged = futuresStagedCross
			}
			h, _, runner, mission, data := staged(t)
			seedAgedBulletinFixture(t, h, runner, mission, &data, store.ActionReplayWindow+time.Hour)
			data.Action.FuturesAttemptedAt = time.Now().Add(-time.Hour)
			contracts := decode[[]gameapi.FuturesContract](runner.snapshot, "futures")
			if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			meta := runner.meta["futures"]
			meta.Complete, meta.Stale, meta.Error = true, false, ""
			meta.AttemptedAt, meta.UpdatedAt = time.Now().UTC(), time.Now().UTC()
			runner.snapshot["futures"], runner.meta["futures"] = encode(contracts), meta
			beforeClear := data
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed || data.Action != nil {
				t.Fatalf("open listing did not resolve negative observation: %t %v %+v", changed, err, data.Action)
			}
			rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if row.Status != "rejected" {
					t.Fatalf("fixture never observed negative acceptance: %+v", row)
				}
			}
			for i := range contracts {
				contract := &contracts[i]
				ours, counterparty := h.account.CompanyID, contract.CreatorCompanyID
				contract.Status = "active"
				if contract.Side == "bid" {
					contract.SellerCompanyID, contract.BuyerCompanyID = &ours, &counterparty
					contract.SellerMarginPence = contract.StrikePricePence * int64(contract.LotTons) / 4
				} else {
					contract.BuyerCompanyID, contract.SellerCompanyID = &ours, &counterparty
					contract.BuyerMarginPence = contract.StrikePricePence * int64(contract.LotTons) / 4
				}
			}
			// The authoritative listing arrives after rejection, including a
			// crash that left the old negative-proof mission to be cleared.
			runner.snapshot["futures"] = encode(contracts)
			if err := runner.observeFutures(h.ctx, rows); err != nil {
				t.Fatal(err)
			}
			data = beforeClear
			if err := runner.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed || data.Action != nil {
				t.Fatalf("late acceptance lost behind durable rejection: %t %v %+v", changed, err, data.Action)
			}
			rows, err = h.db.FuturesIntents(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if row.Status != "active" || row.FuturesID != row.ListingID || len(row.Outcome) != 0 ||
					row.Side == "ask" && row.ReservedTons != row.LotTons {
					t.Fatalf("late commitment was not readopted with its original coverage: %+v", row)
				}
			}
			if batch {
				if reserve, err := h.db.FuturesCashReserve(h.ctx, h.account.ID); err != nil || reserve < 1000 {
					t.Fatalf("late purchase lost its strike/rent reserve: %d %v", reserve, err)
				}
			}
		})
	}
}
