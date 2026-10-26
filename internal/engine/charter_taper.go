package engine

import "math"

// Crown and Admiralty charters pay the goods' value in full, but the premium
// above it is multiplied by sqrt(threshold / earnings) once charter earnings
// over the last 10 game days, this delivery included, pass 50,000 Crowns.
const (
	charterTaperThresholdPence = 5_000_000
	charterTaperWindowGameDays = 10
)

// taperedCharterReward returns the per-ton pay a Crown charter delivery of up
// to deliveryTons is expected to earn. Player charters are never tapered. An
// unknown catalog price treats the whole reward as premium, which only lowers
// the estimate.
func taperedCharterReward(contract Contract, catalog Catalog, earnedPence, deliveryTons int64) int64 {
	if contract.RewardPerTonPence == nil {
		return 0
	}
	reward := *contract.RewardPerTonPence
	if contract.ContractType != "charter" || contract.IssuerCompanyID != "" || reward <= 0 {
		return reward
	}
	var value int64
	for _, commodity := range catalog.Commodities {
		if commodity.ID == contract.CommodityID {
			value = min(max(0, commodity.BasePricePence), reward)
			break
		}
	}
	delivery := checkedPenceProduct(max(0, min(contract.RemainingQuantity, deliveryTons)), reward)
	earnings := float64(max(0, earnedPence)) + float64(delivery)
	if earnings <= charterTaperThresholdPence {
		return reward
	}
	premium := float64(reward - value)
	return value + int64(math.Floor(premium*math.Sqrt(charterTaperThresholdPence/earnings)))
}

// taperedContracts applies the Crown charter premium taper to every charter
// the ship could deliver, copying the slice only when a reward changes.
func taperedContracts(contracts []Contract, catalog Catalog, earnedPence, deliveryTons int64) []Contract {
	var tapered []Contract
	for index, contract := range contracts {
		if contract.RewardPerTonPence == nil {
			continue
		}
		reward := taperedCharterReward(contract, catalog, earnedPence, deliveryTons)
		if reward == *contract.RewardPerTonPence {
			continue
		}
		if tapered == nil {
			tapered = append([]Contract(nil), contracts...)
		}
		tapered[index].RewardPerTonPence = &reward
		tapered[index].NominalRewardPerTonPence = contract.RewardPerTonPence
	}
	if tapered == nil {
		return contracts
	}
	return tapered
}

// nominalCharterReward is the posted per-ton reward, before any taper.
func nominalCharterReward(contract Contract) int64 {
	if contract.NominalRewardPerTonPence != nil {
		return *contract.NominalRewardPerTonPence
	}
	if contract.RewardPerTonPence != nil {
		return *contract.RewardPerTonPence
	}
	return 0
}
