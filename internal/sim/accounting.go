package sim

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

type Money struct {
	AcquisitionPence int64 `json:"acquisition_pence"`
	ProceedsPence    int64 `json:"proceeds_pence"`
	FeesPence        int64 `json:"fees_pence"`
	ProvisionsPence  int64 `json:"provisions_pence"`
	ExpensesPence    int64 `json:"expenses_pence"`
}

func (m Money) Net() int64 {
	return m.ProceedsPence - m.AcquisitionPence - m.FeesPence - m.ProvisionsPence - m.ExpensesPence
}

func settlementMoney(row store.Settlement) Money {
	return Money{row.AcquisitionPence, row.ProceedsPence, row.FeesPence, row.ProvisionsPence, row.ExpensesPence}
}

func (m Money) minus(other Money) Money {
	return Money{m.AcquisitionPence - other.AcquisitionPence, m.ProceedsPence - other.ProceedsPence, m.FeesPence - other.FeesPence, m.ProvisionsPence - other.ProvisionsPence, m.ExpensesPence - other.ExpensesPence}
}

// ApplyReceipt deliberately matches engine.consumeResult, including provision
// customs and the exclusion of remittance principal and returned collateral.
func ApplyReceipt(money *Money, stage string, response json.RawMessage) error {
	var receipt struct {
		Filled        *int64 `json:"filled_quantity"`
		Gross         *int64 `json:"total_gross_pence"`
		Customs       *int64 `json:"customs_duty_pence"`
		Reward        *int64 `json:"reward_paid_pence"`
		Fee           *int64 `json:"fee_pence"`
		Uninterpreted bool   `json:"committed_uninterpreted"`
	}
	if json.Unmarshal(response, &receipt) != nil || receipt.Uninterpreted {
		return errors.New("missing interpretable receipt")
	}
	switch stage {
	case "buy", "sell", "provision":
		if receipt.Filled == nil || receipt.Gross == nil || receipt.Customs == nil || *receipt.Filled < 0 || *receipt.Gross < 0 || *receipt.Customs < 0 {
			return errors.New("incomplete trade receipt")
		}
		switch stage {
		case "buy":
			money.AcquisitionPence += *receipt.Gross
			money.FeesPence += *receipt.Customs
		case "sell":
			money.ProceedsPence += *receipt.Gross
			money.FeesPence += *receipt.Customs
		case "provision":
			money.ProvisionsPence += *receipt.Gross + *receipt.Customs
		}
	case "deliver-charter", "deliver-freight":
		if receipt.Reward == nil || *receipt.Reward < 0 {
			return errors.New("incomplete delivery receipt")
		}
		money.ProceedsPence += *receipt.Reward
	case "remit", "sourcing-remit":
		if receipt.Fee == nil || *receipt.Fee < 0 {
			return errors.New("incomplete remittance receipt")
		}
		money.FeesPence += *receipt.Fee
	case "depart", "dock", "offshore", "accept-freight", "hire-factor", "dismiss-factor", "settings", "build", "refit", "loan-accept", "loan-repay", "marque", "underwrite":
		// Departure dues are ledger bills, not a second receipt cost.
	default:
		return fmt.Errorf("unsupported receipt stage %s", stage)
	}
	return nil
}

type Bill struct {
	ID          string    `json:"id"`
	Region      string    `json:"region"`
	Reason      string    `json:"reason"`
	Owner       string    `json:"owner,omitempty"`
	At          time.Time `json:"at"`
	AmountPence int64     `json:"amount_pence"`
}

type missionAccount struct {
	settlement store.Settlement
	money      Money
}

type Accounting struct {
	initial  int64
	vaults   map[string]int64
	missions map[string]*missionAccount
	bills    []Bill
	settled  []store.Settlement
}

func NewAccounting(vaults []engine.Vault) *Accounting {
	a := &Accounting{vaults: make(map[string]int64), missions: make(map[string]*missionAccount)}
	for _, vault := range vaults {
		if vault.AccountType != "" && vault.AccountType != "vault" {
			continue
		}
		a.vaults[vault.BankingRegion] += vault.BalancePence
		a.initial += vault.BalancePence
	}
	return a
}

func (a *Accounting) Balances() map[string]int64 { return cloneMap(a.vaults) }

func (a *Accounting) Start(settlement store.Settlement) {
	if _, exists := a.missions[settlement.MissionID]; exists {
		return
	}
	a.missions[settlement.MissionID] = &missionAccount{settlement: settlement, money: settlementMoney(settlement)}
}

func (a *Accounting) Receipt(missionID, region, stage string, response json.RawMessage) error {
	mission := a.missions[missionID]
	if mission == nil {
		return errors.New("receipt has no open mission")
	}
	next := mission.money
	if err := ApplyReceipt(&next, stage, response); err != nil {
		return err
	}
	delta := next.Net() - mission.money.Net()
	if delta < 0 && a.vaults[region] < -delta {
		return errors.New("insufficient regional cash")
	}
	a.vaults[region] += delta
	mission.money = next
	return nil
}

func (a *Accounting) Remit(missionID, from, to string, gross, fee int64) error {
	mission := a.missions[missionID]
	if mission == nil || from == to || gross <= 0 || fee < 0 || fee > gross || a.vaults[from] < gross {
		return errors.New("invalid funded remittance")
	}
	a.vaults[from] -= gross
	a.vaults[to] += gross - fee
	mission.money.FeesPence += fee
	return nil
}

func (a *Accounting) Charge(bill Bill) error {
	if bill.ID == "" || bill.AmountPence < 0 {
		return errors.New("invalid overhead bill")
	}
	for _, prior := range a.bills {
		if prior.ID == bill.ID {
			return errors.New("duplicate overhead bill")
		}
	}
	// Mandatory bills are liabilities even if a scenario exhausts cash.
	// They must remain visible in net worth, not disappear behind a cash cap.
	a.vaults[bill.Region] -= bill.AmountPence
	a.bills = append(a.bills, bill)
	return nil
}

func (a *Accounting) Settle(missionID string, at time.Time) (store.Settlement, error) {
	mission := a.missions[missionID]
	if mission == nil || at.Before(mission.settlement.StartedAt) {
		return store.Settlement{}, errors.New("invalid mission settlement")
	}
	for i := range a.bills {
		bill := &a.bills[i]
		if bill.Owner != "" || bill.At.Before(mission.settlement.StartedAt) || bill.At.After(at) {
			continue
		}
		for _, reason := range []string{"upkeep", "rent", "berth", "toll"} {
			if strings.Contains(bill.Reason, reason) {
				bill.Owner = missionID
				mission.money.ExpensesPence += bill.AmountPence
				break
			}
		}
	}
	row := mission.settlement
	row.AcquisitionPence = mission.money.AcquisitionPence
	row.ProceedsPence = mission.money.ProceedsPence
	row.FeesPence = mission.money.FeesPence
	row.ProvisionsPence = mission.money.ProvisionsPence
	row.ExpensesPence = mission.money.ExpensesPence
	row.CompletedAt = at
	// ExplorationExpensePence is attribution of already-paid costs. It is
	// reported on the row but deliberately absent from Money.Net.
	a.settled = append(a.settled, row)
	delete(a.missions, missionID)
	return row, nil
}

type CashReconciliation struct {
	VaultDeltaPence        int64 `json:"vault_delta_pence"`
	SettledNetPence        int64 `json:"settled_net_pence"`
	OpenMissionNetPence    int64 `json:"open_mission_net_pence"`
	UnclaimedOverheadPence int64 `json:"unclaimed_overhead_pence"`
}

func (a *Accounting) Reconcile() (CashReconciliation, error) {
	r := CashReconciliation{VaultDeltaPence: -a.initial}
	for _, balance := range a.vaults {
		r.VaultDeltaPence += balance
	}
	for _, row := range a.settled {
		r.SettledNetPence += settlementMoney(row).Net()
	}
	for _, mission := range a.missions {
		r.OpenMissionNetPence += mission.money.Net()
	}
	for _, bill := range a.bills {
		if bill.Owner == "" {
			r.UnclaimedOverheadPence += bill.AmountPence
		}
	}
	if r.VaultDeltaPence != r.SettledNetPence+r.OpenMissionNetPence-r.UnclaimedOverheadPence {
		return r, errors.New("cash accounting does not reconcile")
	}
	return r, nil
}

func NetWorth(vaults map[string]int64, ships []engine.ShipState, catalog engine.Catalog) int64 {
	prices := make(map[string]int64, len(catalog.Commodities))
	for _, commodity := range catalog.Commodities {
		prices[commodity.ID] = commodity.BasePricePence
	}
	var worth int64
	for _, cash := range vaults {
		worth += cash
	}
	for _, ship := range ships {
		for commodity, tons := range ship.CargoManifest {
			worth += tons * prices[commodity]
		}
	}
	return worth
}

func (a *Accounting) Settlements() []store.Settlement {
	rows := append([]store.Settlement(nil), a.settled...)
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CompletedAt.Equal(rows[j].CompletedAt) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].CompletedAt.Before(rows[j].CompletedAt)
	})
	return rows
}
