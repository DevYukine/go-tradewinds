package store

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Length framing keeps accounts, purposes and external identities unambiguous.
func privateFrame(parts ...string) string {
	var framed []byte
	for _, part := range parts {
		framed = binary.BigEndian.AppendUint64(framed, uint64(len(part)))
		framed = append(framed, part...)
	}
	return string(framed)
}

func deriveLookupKey(masterKey []byte) []byte {
	mac := hmac.New(sha256.New, masterKey)
	_, _ = mac.Write([]byte(privateFrame("tradewinds", "private-evidence", "lookup-key", "v1")))
	return mac.Sum(nil)
}

func (s *Store) privateLookup(purpose, accountID, identity string) string {
	mac := hmac.New(sha256.New, s.lookupKey)
	_, _ = mac.Write([]byte(privateFrame("tradewinds", "private-evidence", "v1", purpose, accountID, identity)))
	return hex.EncodeToString(mac.Sum(nil))
}

func privateAAD(table, field, accountID, identity string) string {
	return privateFrame("tradewinds", "private-evidence", "v1", table, field, accountID, identity)
}

func (s *Store) encryptPrivate(value any, table, field, accountID, identity string) ([]byte, error) {
	plain, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode private evidence: %w", err)
	}
	return s.encrypt(plain, privateAAD(table, field, accountID, identity))
}

func (s *Store) decodePrivate(ciphertext []byte, value any, table, field, accountID, identity string) error {
	plain, err := s.decrypt(ciphertext, privateAAD(table, field, accountID, identity))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(plain, value); err != nil {
		return fmt.Errorf("decode private evidence: %w", err)
	}
	return nil
}

type missionRow struct {
	AccountID      string `gorm:"primaryKey"`
	ShipLookup     string `gorm:"primaryKey"`
	MetadataCipher []byte
	DataCipher     []byte
	UpdatedAt      time.Time
}

func (missionRow) TableName() string { return "missions" }

type missionMetadata struct {
	ShipID      string
	Phase       string
	Destination string
	Commodity   string
	ContractID  string
	Quantity    int64
	PricePence  int64
	Generation  string
}

func (s *Store) encodeMission(mission Mission) (missionRow, error) {
	row := missionRow{
		AccountID:  mission.AccountID,
		ShipLookup: s.privateLookup("mission-ship", mission.AccountID, mission.ShipID),
		UpdatedAt:  mission.UpdatedAt,
	}
	metadata := missionMetadata{
		ShipID: mission.ShipID, Phase: mission.Phase, Destination: mission.Destination,
		Commodity: mission.Commodity, ContractID: mission.ContractID, Quantity: mission.Quantity,
		PricePence: mission.PricePence, Generation: mission.Generation,
	}
	var err error
	row.MetadataCipher, err = s.encryptPrivate(metadata, "missions", "metadata", row.AccountID, row.ShipLookup)
	if err != nil {
		return row, err
	}
	// Store recovery bytes without interpreting or re-marshaling engine members.
	row.DataCipher, err = s.encrypt(mission.Data, privateAAD("missions", "data", row.AccountID, row.ShipLookup))
	return row, err
}

func (s *Store) decodeMission(row missionRow) (*Mission, error) {
	var metadata missionMetadata
	if err := s.decodePrivate(row.MetadataCipher, &metadata, "missions", "metadata", row.AccountID, row.ShipLookup); err != nil {
		return nil, err
	}
	if s.privateLookup("mission-ship", row.AccountID, metadata.ShipID) != row.ShipLookup {
		return nil, ErrActionConflict
	}
	data, err := s.decrypt(row.DataCipher, privateAAD("missions", "data", row.AccountID, row.ShipLookup))
	if err != nil {
		return nil, err
	}
	return &Mission{
		AccountID: row.AccountID, ShipID: metadata.ShipID, Phase: metadata.Phase,
		Destination: metadata.Destination, Commodity: metadata.Commodity, ContractID: metadata.ContractID,
		Quantity: metadata.Quantity, PricePence: metadata.PricePence, Generation: metadata.Generation,
		Data: data, UpdatedAt: row.UpdatedAt,
	}, nil
}

type settlementRow struct {
	ID                      string `gorm:"primaryKey"`
	AccountID               string
	MissionID               string
	ActionID                *string
	EventID                 *string
	LaneLookup              string
	ContextCipher           []byte
	Kind                    string
	QuantityTons            int64
	GameSeconds             int64
	CostsKnown              bool
	AcquisitionPence        int64
	ProceedsPence           int64
	ProceedsUnknown         bool
	FeesPence               int64
	ProvisionsPence         int64
	ExpensesPence           int64
	ExplorationExpensePence *int64
	PlannedNetPence         *int64
	PlannedRevenuePence     *int64
	RevenueBasis            *string
	RevenueAgeGameHours     *float64
	HazardBPS               *int64
	HazardSource            *string
	CargoShortfallPence     *int64
	DepartedCargoPence      *int64
	StartedAt               time.Time
	CompletedAt             time.Time
	CreatedAt               time.Time
}

func (settlementRow) TableName() string { return "settlements" }

type settlementContext struct {
	ShipID       string
	Lane         string
	Kind         string
	EventID      string
	Origin       string
	Destination  string
	Commodity    string
	ProbeOutcome string
}

func settlementClassification(kind string) string {
	switch kind {
	case "spot", "charter", "freight", "spot+freight", "charter+freight", "recovery", "passage", "relocation", "aborted", "maintenance":
		return kind
	default:
		return "unknown"
	}
}

func (s *Store) encodeSettlement(value Settlement) (settlementRow, error) {
	row := settlementRow{
		ID: value.ID, AccountID: value.AccountID, MissionID: value.MissionID,
		LaneLookup: s.privateLookup("settlement-lane", "", value.Lane), Kind: settlementClassification(value.Kind),
		QuantityTons: value.QuantityTons, GameSeconds: value.GameSeconds, CostsKnown: value.CostsKnown,
		AcquisitionPence: value.AcquisitionPence, ProceedsPence: value.ProceedsPence,
		ProceedsUnknown: value.ProceedsUnknown,
		FeesPence:       value.FeesPence, ProvisionsPence: value.ProvisionsPence, ExpensesPence: value.ExpensesPence,
		ExplorationExpensePence: value.ExplorationExpensePence,
		PlannedNetPence:         value.PlannedNetPence, PlannedRevenuePence: value.PlannedRevenuePence,
		RevenueBasis: value.RevenueBasis, RevenueAgeGameHours: value.RevenueAgeGameHours,
		HazardBPS: value.HazardBPS, HazardSource: value.HazardSource,
		CargoShortfallPence: value.CargoShortfallPence, DepartedCargoPence: value.DepartedCargoPence,
		StartedAt: value.StartedAt, CompletedAt: value.CompletedAt, CreatedAt: value.CreatedAt,
	}
	if value.ActionID != "" {
		row.ActionID = &value.ActionID
	}
	if value.EventID != "" {
		row.EventID = &value.EventID
	}
	context := settlementContext{
		ShipID: value.ShipID, Lane: value.Lane, Kind: value.Kind, EventID: value.EventID, Origin: value.Origin,
		Destination: value.Destination, Commodity: value.Commodity, ProbeOutcome: value.ProbeOutcome,
	}
	var err error
	row.ContextCipher, err = s.encryptPrivate(context, "settlements", "context", row.AccountID, row.ID)
	return row, err
}

func (s *Store) decodeSettlement(row settlementRow) (Settlement, error) {
	var context settlementContext
	if err := s.decodePrivate(row.ContextCipher, &context, "settlements", "context", row.AccountID, row.ID); err != nil {
		return Settlement{}, err
	}
	var actionID, eventID string
	if row.ActionID != nil {
		actionID = *row.ActionID
	}
	if row.EventID != nil {
		eventID = *row.EventID
	}
	if s.privateLookup("settlement-lane", "", context.Lane) != row.LaneLookup ||
		settlementClassification(context.Kind) != row.Kind || context.EventID != eventID {
		return Settlement{}, ErrActionConflict
	}
	return Settlement{
		ID: row.ID, AccountID: row.AccountID, ShipID: context.ShipID, MissionID: row.MissionID,
		Lane: context.Lane, ActionID: actionID, EventID: eventID, Kind: context.Kind, Origin: context.Origin,
		Destination: context.Destination, Commodity: context.Commodity, ProbeOutcome: context.ProbeOutcome,
		QuantityTons: row.QuantityTons, GameSeconds: row.GameSeconds, CostsKnown: row.CostsKnown,
		AcquisitionPence: row.AcquisitionPence, ProceedsPence: row.ProceedsPence,
		ProceedsUnknown: row.ProceedsUnknown,
		FeesPence:       row.FeesPence, ProvisionsPence: row.ProvisionsPence, ExpensesPence: row.ExpensesPence,
		ExplorationExpensePence: row.ExplorationExpensePence,
		PlannedNetPence:         row.PlannedNetPence, PlannedRevenuePence: row.PlannedRevenuePence,
		RevenueBasis: row.RevenueBasis, RevenueAgeGameHours: row.RevenueAgeGameHours,
		HazardBPS: row.HazardBPS, HazardSource: row.HazardSource,
		CargoShortfallPence: row.CargoShortfallPence, DepartedCargoPence: row.DepartedCargoPence,
		StartedAt: row.StartedAt, CompletedAt: row.CompletedAt, CreatedAt: row.CreatedAt,
	}, nil
}

type passengerEvidenceRow struct {
	ID                     string `gorm:"primaryKey"`
	AccountID              string
	LaneLookup             string
	ContextCipher          []byte
	FarePerHeadPence       int64
	Outcome                string
	Heads                  int64
	GameHour               float64
	LeewayGameHours        *float64
	LateCutPerGameDayPence *int64
	CreatedAt              time.Time
}

func (passengerEvidenceRow) TableName() string { return "passenger_evidence" }

type passengerEvidenceContext struct {
	Lane string
	Kind string
}

func (s *Store) encodePassengerEvidence(value PassengerEvidence) (passengerEvidenceRow, error) {
	row := passengerEvidenceRow{
		ID: value.ID, AccountID: value.AccountID,
		LaneLookup:       s.privateLookup("passenger-lane", value.AccountID, value.Lane),
		FarePerHeadPence: value.FarePerHeadPence, Outcome: value.Outcome, Heads: value.Heads,
		GameHour: value.GameHour, LeewayGameHours: value.LeewayGameHours,
		LateCutPerGameDayPence: value.LateCutPerGameDayPence, CreatedAt: value.CreatedAt,
	}
	var err error
	row.ContextCipher, err = s.encryptPrivate(passengerEvidenceContext{Lane: value.Lane, Kind: value.Kind},
		"passenger_evidence", "context", row.AccountID, row.ID)
	return row, err
}

func (s *Store) decodePassengerEvidence(row passengerEvidenceRow) (PassengerEvidence, error) {
	var context passengerEvidenceContext
	if err := s.decodePrivate(row.ContextCipher, &context, "passenger_evidence", "context", row.AccountID, row.ID); err != nil {
		return PassengerEvidence{}, err
	}
	if s.privateLookup("passenger-lane", row.AccountID, context.Lane) != row.LaneLookup {
		return PassengerEvidence{}, ErrActionConflict
	}
	return PassengerEvidence{
		ID: row.ID, AccountID: row.AccountID, Lane: context.Lane, Kind: context.Kind,
		FarePerHeadPence: row.FarePerHeadPence, Outcome: row.Outcome, Heads: row.Heads,
		GameHour: row.GameHour, LeewayGameHours: row.LeewayGameHours,
		LateCutPerGameDayPence: row.LateCutPerGameDayPence, CreatedAt: row.CreatedAt,
	}, nil
}

type underwritingPolicyRow struct {
	AccountID       string `gorm:"primaryKey"`
	PolicyLookup    string `gorm:"primaryKey"`
	IdentityCipher  []byte
	PayloadCipher   []byte
	Status          string
	SourceUpdatedAt time.Time
	ObservedAt      time.Time
	TermsChanged    bool
}

func (underwritingPolicyRow) TableName() string { return "underwriting_policies" }

type underwritingPolicyIdentity struct {
	PolicyID  string
	TermsHash string
}

type underwritingPolicyPayload struct {
	Status  string
	Payload []byte
}

func policyClassification(status string) string {
	switch status {
	case "open", "active", "settled_safe", "claimed_loss", "cancelled", "terms_changed":
		return status
	default:
		return "unknown"
	}
}

func (s *Store) encodeUnderwritingPolicy(value UnderwritingPolicyRecord) (underwritingPolicyRow, error) {
	row := underwritingPolicyRow{
		AccountID: value.AccountID, PolicyLookup: s.privateLookup("underwriting-policy", value.AccountID, value.PolicyID),
		Status: policyClassification(value.Status), SourceUpdatedAt: value.SourceUpdatedAt,
		ObservedAt: value.ObservedAt, TermsChanged: value.TermsChanged,
	}
	var err error
	row.IdentityCipher, err = s.encryptPrivate(underwritingPolicyIdentity{PolicyID: value.PolicyID, TermsHash: value.TermsHash},
		"underwriting_policies", "identity", row.AccountID, row.PolicyLookup)
	if err != nil {
		return row, err
	}
	row.PayloadCipher, err = s.encryptPrivate(underwritingPolicyPayload{Status: value.Status, Payload: value.Payload},
		"underwriting_policies", "payload", row.AccountID, row.PolicyLookup)
	return row, err
}

func (s *Store) decodeUnderwritingPolicy(row underwritingPolicyRow) (UnderwritingPolicyRecord, error) {
	var identity underwritingPolicyIdentity
	if err := s.decodePrivate(row.IdentityCipher, &identity, "underwriting_policies", "identity", row.AccountID, row.PolicyLookup); err != nil {
		return UnderwritingPolicyRecord{}, err
	}
	var payload underwritingPolicyPayload
	if err := s.decodePrivate(row.PayloadCipher, &payload, "underwriting_policies", "payload", row.AccountID, row.PolicyLookup); err != nil {
		return UnderwritingPolicyRecord{}, err
	}
	if s.privateLookup("underwriting-policy", row.AccountID, identity.PolicyID) != row.PolicyLookup || policyClassification(payload.Status) != row.Status {
		return UnderwritingPolicyRecord{}, ErrActionConflict
	}
	return UnderwritingPolicyRecord{
		AccountID: row.AccountID, PolicyID: identity.PolicyID, TermsHash: identity.TermsHash, Status: payload.Status,
		Payload: payload.Payload, SourceUpdatedAt: row.SourceUpdatedAt, ObservedAt: row.ObservedAt, TermsChanged: row.TermsChanged,
	}, nil
}

type underwritingPositionRow struct {
	AccountID               string `gorm:"primaryKey"`
	PositionLookup          string `gorm:"primaryKey"`
	ActionID                string
	IdentityCipher          []byte
	FrozenCipher            []byte
	SliceBPS                int64
	CollateralPence         int64
	PremiumPence            int64
	FundingEvidence         json.RawMessage `gorm:"type:jsonb"`
	AcceptedAt              time.Time
	Status                  string
	EarnedPremiumPence      *int64
	ClaimsPence             *int64
	ReturnedCollateralPence *int64
	FeesPence               *int64
	NetPence                *int64
	ClosedAt                *time.Time
	ClosureEvidence         json.RawMessage `gorm:"type:jsonb"`
}

func (underwritingPositionRow) TableName() string { return "underwriting_positions" }

type underwritingPositionIdentity struct {
	ID             string
	PolicyID       string
	CompanyID      string
	OriginalStatus string
}

func positionClassification(status string) string {
	switch status {
	case "active", "settled_safe", "claimed_loss", "cancelled":
		return status
	default:
		return "unknown"
	}
}

func (s *Store) encodeUnderwritingPosition(value UnderwritingPosition) (underwritingPositionRow, error) {
	row := underwritingPositionRow{
		AccountID: value.AccountID, PositionLookup: s.privateLookup("underwriting-position", value.AccountID, value.ID),
		ActionID: value.ActionID, SliceBPS: value.SliceBPS, CollateralPence: value.CollateralPence, PremiumPence: value.PremiumPence,
		FundingEvidence: value.FundingEvidence, AcceptedAt: value.AcceptedAt, Status: positionClassification(value.Status),
		EarnedPremiumPence: value.EarnedPremiumPence, ClaimsPence: value.ClaimsPence, ReturnedCollateralPence: value.ReturnedCollateralPence,
		FeesPence: value.FeesPence, NetPence: value.NetPence, ClosedAt: value.ClosedAt,
	}
	identity := underwritingPositionIdentity{ID: value.ID, PolicyID: value.PolicyID, CompanyID: value.CompanyID, OriginalStatus: value.Status}
	var err error
	row.IdentityCipher, err = s.encryptPrivate(identity, "underwriting_positions", "identity", row.AccountID, row.PositionLookup)
	if err != nil {
		return row, err
	}
	row.FrozenCipher, err = s.encrypt(value.FrozenPolicy, privateAAD("underwriting_positions", "frozen", row.AccountID, row.PositionLookup))
	return row, err
}

func (s *Store) decodeUnderwritingPosition(row underwritingPositionRow) (UnderwritingPosition, error) {
	var identity underwritingPositionIdentity
	if err := s.decodePrivate(row.IdentityCipher, &identity, "underwriting_positions", "identity", row.AccountID, row.PositionLookup); err != nil {
		return UnderwritingPosition{}, err
	}
	if s.privateLookup("underwriting-position", row.AccountID, identity.ID) != row.PositionLookup {
		return UnderwritingPosition{}, ErrActionConflict
	}
	frozen, err := s.decrypt(row.FrozenCipher, privateAAD("underwriting_positions", "frozen", row.AccountID, row.PositionLookup))
	if err != nil {
		return UnderwritingPosition{}, err
	}
	status := row.Status
	if status == "unknown" {
		status = identity.OriginalStatus
	}
	return UnderwritingPosition{
		ID: identity.ID, AccountID: row.AccountID, ActionID: row.ActionID, PolicyID: identity.PolicyID, CompanyID: identity.CompanyID,
		FrozenPolicy: frozen, SliceBPS: row.SliceBPS, CollateralPence: row.CollateralPence, PremiumPence: row.PremiumPence,
		FundingEvidence: row.FundingEvidence, AcceptedAt: row.AcceptedAt, Status: status,
		EarnedPremiumPence: row.EarnedPremiumPence, ClaimsPence: row.ClaimsPence, ReturnedCollateralPence: row.ReturnedCollateralPence,
		FeesPence: row.FeesPence, NetPence: row.NetPence, ClosedAt: row.ClosedAt,
	}, nil
}
