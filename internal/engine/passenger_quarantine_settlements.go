package engine

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func passengerQuarantineSettlement(archive *store.Mission, original *missionData) *store.Settlement {
	return &store.Settlement{
		ID:        actionID(archive.AccountID, archive.Generation, "passenger-quarantine-accounting", 0),
		AccountID: archive.AccountID, ShipID: original.Plan.ShipID, MissionID: archive.Generation,
		Kind: "passage", Lane: original.Plan.Origin + "|" + original.Plan.Destination, Origin: original.Plan.Origin, Destination: original.Plan.Destination,
		CostsKnown: false, ProceedsUnknown: true, RevenueBasis: new("quarantined_passage_accounting"),
		AcquisitionPence: max(0, original.AcquisitionPence), FeesPence: max(0, original.FeesPence), ProvisionsPence: max(0, original.ProvisionsPence), ExpensesPence: max(0, original.ExpensesPence),
		StartedAt: original.StartedAt, CompletedAt: archive.UpdatedAt,
	}
}

func (r *Runner) recordPassengerQuarantineSettlement(ctx context.Context, archive *store.Mission) error {
	var original missionData
	if err := json.Unmarshal(archive.Data, &original); err != nil {
		return err
	}
	marker := passengerQuarantineSettlement(archive, &original)
	// Freeze the residual at first quarantine. Replays must not incorporate
	// later recovery receipts or change an already durable accounting marker.
	existing, err := r.db.SettlementsByID(ctx, archive.AccountID, []string{marker.ID})
	if err == nil {
		marker.AcquisitionPence, marker.FeesPence = existing[0].AcquisitionPence, existing[0].FeesPence
		marker.ProvisionsPence, marker.ExpensesPence = existing[0].ProvisionsPence, existing[0].ExpensesPence
	} else if errors.Is(err, store.ErrNotFound) {
		carried, err := r.db.PassengerSettlementCosts(ctx, archive.AccountID, archive.Generation)
		if err != nil {
			return err
		}
		marker.AcquisitionPence = max(0, marker.AcquisitionPence-carried.AcquisitionPence)
		marker.FeesPence = max(0, marker.FeesPence-carried.FeesPence)
		marker.ProvisionsPence = max(0, marker.ProvisionsPence-carried.ProvisionsPence)
		marker.ExpensesPence = max(0, marker.ExpensesPence-carried.ExpensesPence)
	} else {
		return err
	}
	return r.db.PutPassengerQuarantineSettlement(ctx, marker)
}

// Recovery shares the passage's identity but not a second copy of its costs.
// Its terminal marker records only additional known debits; all fare proceeds
// and the full cost basis remain explicitly unknown and cannot contribute R.
func (r *Runner) recordPassengerRecoverySettlement(ctx context.Context, mission *store.Mission, data *missionData) error {
	if data.Settlement == nil || data.Settlement.RevenueBasis == nil || *data.Settlement.RevenueBasis != "quarantined_passage_recovery" {
		archive, err := r.db.PassengerQuarantineArchive(ctx, r.account.ID, mission.Generation)
		if err != nil {
			return err
		}
		if archive == nil {
			return store.ErrNotFound
		}
		var original missionData
		if err := json.Unmarshal(archive.Data, &original); err != nil {
			return err
		}
		bills, ids := r.claimBillsSince(data, mission.Generation)
		data.ExpensesPence = growthCashTotal(data.ExpensesPence, bills)
		data.SettlementExpenseIDs = append(data.SettlementExpenseIDs, ids...)
		completed := time.Now().UTC()
		marker := passengerQuarantineSettlement(archive, &original)
		marker.ID = actionID(r.account.ID, mission.Generation, "passenger-quarantine-recovery", 0)
		marker.Origin, marker.Destination = data.Plan.Origin, data.Plan.Destination
		marker.Lane = marker.Origin + "|" + marker.Destination
		marker.RevenueBasis = new("quarantined_passage_recovery")
		marker.AcquisitionPence = max(0, max(0, data.AcquisitionPence)-max(0, original.AcquisitionPence))
		marker.FeesPence = max(0, max(0, data.FeesPence)-max(0, original.FeesPence))
		marker.ProvisionsPence = max(0, max(0, data.ProvisionsPence)-max(0, original.ProvisionsPence))
		marker.ExpensesPence = max(0, max(0, data.ExpensesPence)-max(0, original.ExpensesPence))
		marker.StartedAt, marker.CompletedAt = archive.UpdatedAt, completed
		data.Settlement = marker
		if err := r.saveMission(ctx, mission, data); err != nil {
			return err
		}
	}
	return r.db.PutPassengerQuarantineSettlement(ctx, data.Settlement)
}
