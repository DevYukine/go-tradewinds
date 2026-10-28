package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gorm.io/gorm"
)

// JournalRow projects recorded company activity, not a cash ledger. Event bills,
// action cash movements and settlement net can describe overlapping money.
// Events retain seven days; actions and settlements are durable.
type JournalRow struct {
	ID               string    `json:"id"`
	AccountID        string    `json:"account_id"`
	At               time.Time `json:"at"`
	Kind             string    `json:"kind"`
	ShipID           string    `json:"ship_id,omitempty"`
	Summary          string    `json:"summary"`
	AmountPence      *int64    `json:"amount_pence,omitempty"`
	AmountKind       string    `json:"amount_kind,omitempty"`
	CostsKnown       *bool     `json:"costs_known,omitempty"`
	Origin           string    `json:"origin,omitempty"`
	Destination      string    `json:"destination,omitempty"`
	Commodity        string    `json:"commodity,omitempty"`
	QuantityTons     *int64    `json:"quantity_tons,omitempty"`
	Heads            *int64    `json:"heads,omitempty"`
	FarePerHeadPence *int64    `json:"fare_per_head_pence,omitempty"`
	FullFarePence    *int64    `json:"full_fare_pence,omitempty"`
	FarePaidPence    *int64    `json:"fare_paid_pence,omitempty"`
	GameDaysLate     *float64  `json:"game_days_late,omitempty"`
	Cause            string    `json:"cause,omitempty"`
	CargoLossPct     *float64  `json:"cargo_loss_pct,omitempty"`
	StoresLossPct    *float64  `json:"stores_loss_pct,omitempty"`
}

// journalOutcomeCiphertextLimit bounds how much ciphertext one journal outcome
// may materialize. Durable responses beyond the budget stay untouched on disk;
// their rows honestly report that outcome details are unavailable.
const journalOutcomeCiphertextLimit = 128 << 10

func (s *Store) journalHistory(ctx context.Context, accountID string, limit int) ([]JournalRow, error) {
	db, cancel := s.query(ctx)
	defer cancel()
	var events []Event
	var settlements []Settlement
	var actions []actionRow
	var rows []JournalRow
	err := db.Transaction(func(tx *gorm.DB) error {
		scrubber := displayScrubber{store: s, tx: tx}
		var account Account
		if err := tx.Select("id, company_id").Where("id = ?", accountID).Take(&account).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		var err error
		settlements, err = s.settlementRows(tx.Where("account_id = ?", accountID).Order("completed_at DESC, id DESC").Limit(limit))
		if err != nil {
			return err
		}
		if account.CompanyID != "" {
			// The stored SSE envelope is payload; only its explicit company scope
			// establishes ownership. Public notices must be filtered before LIMIT.
			if err := tx.Select("account_id, event_id, event_type, created_at, payload -> 'payload' AS payload").
				Where("account_id = ? AND jsonb_typeof(payload -> 'company_id') = 'string' AND payload ->> 'company_id' = ?", accountID, account.CompanyID).
				Order("created_at DESC, event_id DESC").Limit(limit).Find(&events).Error; err != nil {
				return err
			}
		}
		// Whitelist before bounding/decrypting. Departure already has an SSE
		// record; rejected, pending, reconciled and authentication actions stay out.
		// Metadata only: each outcome's ciphertexts are fetched one row at a
		// time below, inside this same repeatable-read snapshot.
		if err := tx.Select("id, account_id, method, path, updated_at").
			Where("account_id = ? AND state = ? AND status >= 200 AND status < 300", accountID, ActionComplete).
			Where("((method = ? AND path IN ?) OR (method = ? AND path = ?))", "POST", []string{
				"/v1/market/place-order",
				"/v1/contract/accept-freight",
				"/v1/contract/deliver-freight",
				"/v1/contract/deliver-charter",
				"/v1/fleet/dock",
				"/v1/fleet/offshore",
				"/v1/shipyard/build-ship",
				"/v1/shipyard/modify-ship",
				"/v1/treasury/remit",
				"/v1/passengers/offer",
				"/v1/passengers/cancel-offer",
			}, "PATCH", "/v1/company/settings").
			Order("updated_at DESC, id DESC").Limit(limit).Find(&actions).Error; err != nil {
			return err
		}

		rows = make([]JournalRow, 0, limit)
		// Fetch, decrypt and project one bounded outcome at a time so each
		// row's ciphertext bytes are unreachable again before the next fetch.
		projectOutcome := func(meta actionRow) (JournalRow, error) {
			var record actionRow
			err := tx.Select("id, account_id, method, path, updated_at, body_cipher, response_cipher").
				Where("id = ? AND octet_length(body_cipher) + octet_length(response_cipher) <= ?", meta.ID, journalOutcomeCiphertextLimit).
				Take(&record).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return JournalRow{}, err
			}
			// A missing row means the durable response exceeded the budget;
			// report honest unavailable metadata instead of materializing it.
			oversized := err != nil
			action := &Action{
				ID: meta.ID, AccountID: meta.AccountID,
				Method: meta.Method, Path: meta.Path, UpdatedAt: meta.UpdatedAt,
			}
			if !oversized {
				credentials := scrubber.credentials(accountID)
				if credentials.err != nil {
					return JournalRow{}, credentials.err
				}
				if action, err = s.decodeAction(record); err != nil {
					return JournalRow{}, err
				}
			}
			if oversized {
				return journalActionRow(action, nil), nil
			}
			row := journalActionRow(action, scrubber.credentials(accountID).secrets)
			scrubber.journal(&row)
			return row, nil
		}
		eventIndex, settlementIndex, actionIndex := 0, 0, 0
		for len(rows) < limit {
			source := ""
			var at time.Time
			// At equal timestamps, prefixed IDs descend settlement, event, action.
			// Each bounded source is already ordered by its timestamp and source ID.
			if settlementIndex < len(settlements) {
				source = "settlement"
				at = settlements[settlementIndex].CompletedAt
			}
			if eventIndex < len(events) && (source == "" || events[eventIndex].CreatedAt.After(at)) {
				source = "event"
				at = events[eventIndex].CreatedAt
			}
			if actionIndex < len(actions) && (source == "" || actions[actionIndex].UpdatedAt.After(at)) {
				source = "action"
			}
			switch source {
			case "settlement":
				scrubber.settlement(&settlements[settlementIndex])
				row, err := journalSettlementRow(settlements[settlementIndex])
				if err != nil {
					return err
				}
				scrubber.journal(&row)
				rows = append(rows, row)
				settlementIndex++
			case "event":
				row := journalEventRow(events[eventIndex])
				// Events are redacted diagnostics, not decrypted outcomes. Do not
				// require credentials for an event that excludes an older action.
				rows = append(rows, row)
				eventIndex++
			case "action":
				row, err := projectOutcome(actions[actionIndex])
				if err != nil {
					return err
				}
				rows = append(rows, row)
				actionIndex++
			default:
				return nil
			}
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func journalSettlementRow(settlement Settlement) (JournalRow, error) {
	row := JournalRow{
		ID:           "settlement:" + settlement.ID,
		AccountID:    settlement.AccountID,
		At:           settlement.CompletedAt,
		Kind:         "settlement",
		ShipID:       settlement.ShipID,
		Summary:      "Voyage settled",
		CostsKnown:   &settlement.CostsKnown,
		Origin:       settlement.Origin,
		Destination:  settlement.Destination,
		Commodity:    settlement.Commodity,
		QuantityTons: &settlement.QuantityTons,
	}
	if settlement.Kind != "" {
		row.Summary = journalLabel(strings.ReplaceAll(settlement.Kind, "+", " and ")) + " voyage settled"
	}
	if settlement.ProceedsUnknown {
		row.Summary += "; proceeds unknown"
		return row, nil
	}
	if !settlement.CostsKnown {
		row.Summary += "; costs unknown"
		return row, nil
	}
	net := settlement.ProceedsPence
	for _, cost := range []int64{settlement.AcquisitionPence, settlement.FeesPence, settlement.ProvisionsPence, settlement.ExpensesPence} {
		if cost < 0 || net < math.MinInt64+cost {
			return JournalRow{}, fmt.Errorf("stored settlement net exceeds integer pence")
		}
		net -= cost
	}
	row.AmountPence = &net
	row.AmountKind = "realized_net"
	return row, nil
}

func journalEventRow(event Event) JournalRow {
	row := JournalRow{
		ID:        "event:" + event.EventID,
		AccountID: event.AccountID,
		At:        event.CreatedAt,
		Kind:      event.EventType,
		Summary:   journalLabel(event.EventType),
	}
	var payload struct {
		ShipID           string   `json:"ship_id"`
		OriginPortID     string   `json:"origin_port_id"`
		DestPortID       string   `json:"dest_port_id"`
		PortID           string   `json:"port_id"`
		AtNode           string   `json:"at_node"`
		CompletedLeg     *int64   `json:"completed_leg_idx"`
		Status           string   `json:"status"`
		Reason           string   `json:"reason"`
		AmountPence      *int64   `json:"amount_pence"`
		TotalBilledPence *int64   `json:"total_billed_pence"`
		Heads            *int64   `json:"heads"`
		FarePerHeadPence *int64   `json:"fare_per_head_pence"`
		FullFarePence    *int64   `json:"full_fare_pence"`
		FarePaidPence    *int64   `json:"fare_paid_pence"`
		GameDaysLate     *float64 `json:"game_days_late"`
		Cause            string   `json:"cause"`
		CargoLossPct     *float64 `json:"cargo_loss_pct"`
		StoresLossPct    *float64 `json:"stores_loss_pct"`
		Metadata         struct {
			ShipID string `json:"ship_id"`
			PortID string `json:"port_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return row
	}
	switch event.EventType {
	case "ship_departed":
		row.ShipID = payload.ShipID
		row.Origin, row.Destination = payload.OriginPortID, payload.DestPortID
		row.Summary = "Departed"
		if row.Origin != "" {
			row.Summary += " from " + journalLabel(row.Origin)
		}
		if row.Destination != "" {
			row.Summary += " for " + journalLabel(row.Destination)
		}
	case "ship_arrived", "ship_berthed":
		row.ShipID, row.Destination = payload.ShipID, payload.PortID
		row.Summary = "Arrived"
		if event.EventType == "ship_berthed" {
			row.Summary = "Berthed"
		}
		if payload.PortID != "" {
			row.Summary += " at " + journalLabel(payload.PortID)
		}
		if event.EventType == "ship_arrived" && payload.Status == "anchored_roadstead" {
			row.Summary += "; anchored in the roadstead"
		}
	case "ship_leg_completed":
		row.ShipID, row.Destination = payload.ShipID, payload.DestPortID
		row.Summary = "Completed voyage leg"
		if payload.CompletedLeg != nil && *payload.CompletedLeg >= 0 && *payload.CompletedLeg < math.MaxInt64 {
			row.Summary += fmt.Sprintf(" %d", *payload.CompletedLeg+1)
		}
		if payload.AtNode != "" {
			row.Summary += " at " + journalLabel(payload.AtNode)
		}
	case "passengers_boarded", "passengers_paid", "passengers_lost":
		row.ShipID = payload.ShipID
		row.Origin, row.Destination = payload.OriginPortID, payload.DestPortID
		row.Heads = payload.Heads
		row.FarePerHeadPence, row.FullFarePence = payload.FarePerHeadPence, payload.FullFarePence
		if event.EventType == "passengers_boarded" {
			row.Summary = "Passengers boarded"
			if payload.Heads != nil {
				row.Summary += fmt.Sprintf(": %d heads", *payload.Heads)
			}
			if payload.FarePerHeadPence != nil {
				row.Summary += fmt.Sprintf("; fare %d pence per head", *payload.FarePerHeadPence)
			}
			if payload.DestPortID != "" {
				row.Summary += " for " + journalLabel(payload.DestPortID)
			}
		} else if event.EventType == "passengers_paid" {
			row.Summary = "Passenger fares paid"
			row.AmountPence, row.AmountKind = payload.FarePaidPence, "cash_in"
			row.FarePaidPence = payload.FarePaidPence
			row.GameDaysLate = payload.GameDaysLate
			if payload.GameDaysLate != nil {
				row.Summary += fmt.Sprintf("; %g game days late", *payload.GameDaysLate)
			}
		} else {
			row.Summary = "Passengers lost"
			if payload.Cause != "" {
				row.Cause = boundedText(payload.Cause, 160)
				row.Summary += ": " + journalLabel(row.Cause)
			}
		}
	case "voyage_hazard":
		row.ShipID = payload.ShipID
		row.CargoLossPct, row.StoresLossPct = payload.CargoLossPct, payload.StoresLossPct
		if payload.CargoLossPct != nil {
			row.Summary += fmt.Sprintf("; cargo loss %g%%", *payload.CargoLossPct)
		}
		if payload.StoresLossPct != nil {
			row.Summary += fmt.Sprintf("; stores loss %g%%", *payload.StoresLossPct)
		}
	case "ship_provisions_exhausted", "ship_evicted", "ship_mutinied":
		row.ShipID = payload.ShipID
	case "company_upkeep_billed":
		row.Summary = "Company upkeep billed"
		if payload.TotalBilledPence != nil && *payload.TotalBilledPence >= 0 {
			row.AmountPence = payload.TotalBilledPence
		}
	case "tax_and_fee_charged":
		row.ShipID = payload.Metadata.ShipID
		row.Summary = "Tax or fee charged"
		if payload.Reason != "" {
			row.Summary = journalLabel(payload.Reason) + " charged"
		}
		if payload.Metadata.PortID != "" {
			row.Summary += " at " + journalLabel(payload.Metadata.PortID)
		}
		if payload.AmountPence != nil && *payload.AmountPence >= 0 {
			row.AmountPence = payload.AmountPence
		}
	}
	if row.AmountPence != nil && row.AmountKind == "" {
		row.AmountKind = "charge"
	}
	row.Summary = boundedText(row.Summary, 512)
	return row
}

func journalActionRow(action *Action, secrets []string) JournalRow {
	operation := strings.ReplaceAll(action.Path[strings.LastIndex(action.Path, "/")+1:], "-", "_")
	// Scrub before labels change or truncate a credential's original bytes.
	label := func(value string) string {
		return journalLabel(scrubText(value, secrets))
	}
	row := JournalRow{
		ID:        "action:" + action.ID,
		AccountID: action.AccountID,
		At:        action.UpdatedAt,
		Kind:      "action:" + operation,
		Summary:   journalLabel(operation) + " request completed; outcome details unavailable",
	}
	var request struct {
		ShipID         string `json:"ship_id"`
		PortID         string `json:"port_id"`
		CommodityID    string `json:"commodity_id"`
		Side           string `json:"side"`
		Quantity       *int64 `json:"quantity"`
		IntoStores     bool   `json:"into_stores"`
		ContractID     string `json:"contract_id"`
		Name           string `json:"name"`
		ShipClass      string `json:"ship_class"`
		TargetCannons  *int64 `json:"target_cannons_count"`
		FromRegion     string `json:"from_region"`
		ToRegion       string `json:"to_region"`
		GrossPence     *int64 `json:"gross_amount_pence"`
		AutoRemit      *bool  `json:"auto_remit_for_bills"`
		AutoRemitFloor *int64 `json:"auto_remit_min_reserve_pence"`
		Destination    string `json:"dest_port_id"`
		OfferID        string `json:"offer_id"`
		FarePerHead    *int64 `json:"fare_per_head_pence"`
		MaxBerths      *int64 `json:"max_berths"`
	}
	var result struct {
		Uninterpreted  bool   `json:"committed_uninterpreted"`
		ShipID         string `json:"ship_id"`
		CarrierShipID  string `json:"carrier_ship_id"`
		PortID         string `json:"port_id"`
		OriginPortID   string `json:"origin_port_id"`
		DestPortID     string `json:"dest_port_id"`
		CommodityID    string `json:"commodity_id"`
		Side           string `json:"side"`
		Filled         *int64 `json:"filled_quantity"`
		GrossPence     *int64 `json:"total_gross_pence"`
		CustomsPence   *int64 `json:"customs_duty_pence"`
		ContractID     string `json:"contract_id"`
		Quantity       *int64 `json:"quantity"`
		Delivered      *int64 `json:"delivered_quantity"`
		RewardPence    *int64 `json:"reward_paid_pence"`
		Name           string `json:"name"`
		ShipClass      string `json:"ship_class"`
		TotalCostPence *int64 `json:"total_cost_pence"`
		Status         string `json:"status"`
		FromRegion     string `json:"from_region"`
		ToRegion       string `json:"to_region"`
		RemitGross     *int64 `json:"gross_amount_pence"`
		RemitFee       *int64 `json:"fee_pence"`
		RemitNet       *int64 `json:"net_credited_pence"`
		AutoRemit      *bool  `json:"auto_remit_for_bills"`
		AutoRemitFloor *int64 `json:"auto_remit_min_reserve_pence"`
		ID             string `json:"id"`
		FarePerHead    *int64 `json:"fare_per_head_pence"`
		MaxBerths      *int64 `json:"max_berths"`
		Ship           struct {
			ID            string `json:"id"`
			CurrentPortID string `json:"current_port_id"`
			Cannons       *int64 `json:"cannons_count"`
		} `json:"ship"`
	}
	requestErr := json.Unmarshal(action.Body, &request)
	resultErr := json.Unmarshal(action.Response, &result)
	if result.Uninterpreted {
		row.Summary = journalLabel(operation) + " request committed; outcome needs reconciliation"
		return row
	}
	if requestErr != nil || resultErr != nil {
		return row
	}
	switch action.Path {
	case "/v1/passengers/offer":
		if request.ShipID == "" || result.ShipID != request.ShipID || result.DestPortID != request.Destination ||
			request.FarePerHead == nil || result.FarePerHead == nil || *request.FarePerHead <= 0 || *request.FarePerHead != *result.FarePerHead ||
			request.MaxBerths == nil || result.MaxBerths == nil || *request.MaxBerths <= 0 || *request.MaxBerths != *result.MaxBerths {
			return row
		}
		row.ShipID, row.Origin, row.Destination = result.ShipID, result.PortID, result.DestPortID
		row.FarePerHeadPence, row.Heads = result.FarePerHead, result.MaxBerths
		row.Summary = fmt.Sprintf("Passenger offer: up to %d heads for %s at %d pence per head", *result.MaxBerths, label(result.DestPortID), *result.FarePerHead)
	case "/v1/passengers/cancel-offer":
		if request.OfferID == "" || result.ID != request.OfferID || result.ShipID == "" || result.Status == "open" {
			return row
		}
		row.ShipID, row.Origin, row.Destination = result.ShipID, result.PortID, result.DestPortID
		row.Summary = "Passenger offer cancelled"
	case "/v1/market/place-order":
		if request.Quantity == nil || *request.Quantity <= 0 || request.CommodityID == "" ||
			result.CommodityID != request.CommodityID || result.Filled == nil || *result.Filled < 0 || *result.Filled > *request.Quantity ||
			result.GrossPence == nil || *result.GrossPence < 0 || result.CustomsPence == nil || *result.CustomsPence < 0 ||
			request.Side != "bid" && request.Side != "ask" || result.Side != "" && result.Side != request.Side ||
			result.PortID != "" && result.PortID != request.PortID || request.IntoStores && (request.Side != "bid" || request.CommodityID != "provisions") {
			return row
		}
		row.ShipID, row.Origin = request.ShipID, request.PortID
		row.Commodity, row.QuantityTons = result.CommodityID, result.Filled
		verb := "Bought"
		if request.Side == "ask" {
			verb = "Sold"
		} else if request.IntoStores {
			verb = "Stocked stores with"
		}
		row.Summary = fmt.Sprintf("%s %d t of %s", verb, *result.Filled, label(result.CommodityID))
		if *result.Filled == 0 {
			row.Summary = "Order placed; no immediate fill"
		}
		if request.Side == "bid" && *result.GrossPence <= math.MaxInt64-*result.CustomsPence {
			amount := *result.GrossPence + *result.CustomsPence
			row.AmountPence, row.AmountKind = &amount, "cash_out"
		} else if request.Side == "ask" && *result.GrossPence >= *result.CustomsPence {
			amount := *result.GrossPence - *result.CustomsPence
			row.AmountPence, row.AmountKind = &amount, "cash_in"
		}
	case "/v1/contract/deliver-charter", "/v1/contract/deliver-freight":
		if request.ContractID == "" || result.ContractID != request.ContractID || request.ShipID == "" ||
			result.CommodityID == "" || result.Delivered == nil || *result.Delivered < 0 || result.RewardPence == nil || *result.RewardPence < 0 {
			return row
		}
		if action.Path == "/v1/contract/deliver-charter" && (request.Quantity == nil || *request.Quantity < *result.Delivered) {
			return row
		}
		row.ShipID, row.Destination = request.ShipID, result.DestPortID
		row.Commodity, row.QuantityTons = result.CommodityID, result.Delivered
		row.AmountPence, row.AmountKind = result.RewardPence, "cash_in"
		row.Summary = fmt.Sprintf("Delivered %d t of %s; freight reward received", *result.Delivered, label(result.CommodityID))
		if action.Path == "/v1/contract/deliver-charter" {
			row.Destination = result.PortID
			row.Summary = fmt.Sprintf("Delivered %d t of %s; charter reward received", *result.Delivered, label(result.CommodityID))
		}
	case "/v1/contract/accept-freight":
		if request.ContractID == "" || result.ContractID != request.ContractID || request.ShipID == "" || result.CarrierShipID != request.ShipID ||
			result.Quantity == nil || *result.Quantity <= 0 || result.CommodityID == "" {
			return row
		}
		row.ShipID, row.Origin, row.Destination = result.CarrierShipID, result.OriginPortID, result.DestPortID
		row.Commodity, row.QuantityTons = result.CommodityID, result.Quantity
		row.Summary = fmt.Sprintf("Accepted %d t of %s as sealed freight", *result.Quantity, label(result.CommodityID))
	case "/v1/shipyard/build-ship":
		if result.ShipID == "" || request.Name == "" || result.Name != request.Name || request.ShipClass == "" || result.ShipClass != request.ShipClass ||
			result.TotalCostPence == nil || *result.TotalCostPence < 0 {
			return row
		}
		row.ShipID, row.Origin = result.ShipID, request.PortID
		row.Summary = "Built " + boundedText(scrubText(result.Name, secrets), 120) + " (" + label(result.ShipClass) + ")"
		row.AmountPence, row.AmountKind = result.TotalCostPence, "cash_out"
	case "/v1/shipyard/modify-ship":
		if request.ShipID == "" || result.Ship.ID != request.ShipID || request.TargetCannons == nil || *request.TargetCannons < 0 ||
			result.Ship.Cannons == nil || *result.Ship.Cannons != *request.TargetCannons {
			return row
		}
		row.ShipID, row.Origin = result.Ship.ID, result.Ship.CurrentPortID
		row.Summary = fmt.Sprintf("Refitted ship to %d cannons", *result.Ship.Cannons)
	case "/v1/treasury/remit":
		if request.FromRegion == "" || request.ToRegion == "" || result.FromRegion != request.FromRegion || result.ToRegion != request.ToRegion ||
			request.GrossPence == nil || result.RemitGross == nil || *result.RemitGross != *request.GrossPence || *result.RemitGross <= 0 ||
			result.RemitFee == nil || *result.RemitFee < 0 || *result.RemitFee > *result.RemitGross ||
			result.RemitNet == nil || *result.RemitGross-*result.RemitFee != *result.RemitNet {
			return row
		}
		row.Origin, row.Destination = result.FromRegion, result.ToRegion
		row.Summary = fmt.Sprintf("Remitted %d pence from %s to %s; fee shown", *result.RemitGross, label(result.FromRegion), label(result.ToRegion))
		row.AmountPence, row.AmountKind = result.RemitFee, "charge"
	case "/v1/fleet/dock", "/v1/fleet/offshore":
		if request.ShipID == "" || result.ShipID != request.ShipID || result.PortID == "" ||
			result.Status != "berthed" && result.Status != "anchored_roadstead" {
			return row
		}
		row.ShipID, row.Destination = result.ShipID, result.PortID
		row.Summary = "Anchored in the roadstead at " + label(result.PortID)
		if result.Status == "berthed" {
			row.Summary = "Berthed at " + label(result.PortID)
		}
	case "/v1/company/settings":
		if result.AutoRemit == nil || result.AutoRemitFloor == nil || *result.AutoRemitFloor < 0 ||
			request.AutoRemit != nil && *request.AutoRemit != *result.AutoRemit ||
			request.AutoRemitFloor != nil && *request.AutoRemitFloor != *result.AutoRemitFloor {
			return row
		}
		row.Summary = "Company bill-remittance settings updated"
	}
	return row
}

func journalLabel(value string) string {
	value = strings.Join(strings.Fields(strings.ReplaceAll(value, "_", " ")), " ")
	value = boundedText(value, 160)
	if value == "" {
		return "Company activity"
	}
	first, size := utf8.DecodeRuneInString(value)
	return string(unicode.ToUpper(first)) + value[size:]
}
