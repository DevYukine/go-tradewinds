package store

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrNotFound         = errors.New("record not found")
	ErrAccountLocked    = errors.New("account already has an active runner")
	ErrActionConflict   = errors.New("action conflicts with its immutable journal entry")
	ErrActionExpired    = errors.New("pending action is outside the safe idempotency replay window")
	ErrInvalidInput     = errors.New("invalid store input")
	ErrResponseTooLarge = errors.New("stored response exceeds the dashboard size limit")
)

const (
	ActionPending    = "pending"
	ActionComplete   = "complete"
	ActionReconciled = "reconciled"

	// The upstream server retains idempotency keys for 24 hours. Leave an hour
	// for clock skew and an in-flight retry rather than risk a second mutation.
	ActionReplayWindow  = 23 * time.Hour
	DiagnosticRetention = 7 * 24 * time.Hour
	MaxHistoryLimit     = 500

	// MaxSnapshotBytes bounds the encoded account snapshot accepted by SaveSnapshot.
	MaxSnapshotBytes = 8 * 1024 * 1024

	maxDiagnosticBytes        = 32 * 1024
	maxDashboardSnapshotBytes = 16 * 1024 * 1024
	maxDashboardAccounts      = 100
)

type Account struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	CompanyID string    `json:"company_id"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

type Credentials struct {
	Handle           string `json:"handle"`
	Passphrase       string `json:"passphrase"`
	APIKey           string `json:"api_key"`
	UnderworldAPIKey string `json:"underworld_api_key"`
	UnderworldMintID string `json:"underworld_mint_id,omitempty"`
}

type Mission struct {
	AccountID   string          `json:"account_id"`
	ShipID      string          `json:"ship_id"`
	Phase       string          `json:"phase"`
	Destination string          `json:"destination"`
	Commodity   string          `json:"commodity"`
	ContractID  string          `json:"contract_id"`
	Quantity    int64           `json:"quantity"`
	PricePence  int64           `json:"price_pence"`
	Generation  string          `json:"generation"`
	Data        json.RawMessage `json:"data" gorm:"type:jsonb"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

func (Mission) TableName() string { return "missions" }

type Action struct {
	ID         string          `json:"id"`
	AccountID  string          `json:"account_id"`
	Method     string          `json:"method"`
	Path       string          `json:"path"`
	Body       json.RawMessage `json:"body"`
	State      string          `json:"state"`
	Status     int             `json:"status"`
	Response   json.RawMessage `json:"response"`
	RetryAfter *time.Time      `json:"retry_after"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

type Metric struct {
	ID         int64     `json:"id" gorm:"primaryKey"`
	AccountID  string    `json:"account_id"`
	Name       string    `json:"name"`
	Value      float64   `json:"value"`
	RecordedAt time.Time `json:"recorded_at"`
}

func (Metric) TableName() string { return "metrics" }

type Decision struct {
	ID        int64           `json:"id" gorm:"primaryKey"`
	AccountID string          `json:"account_id"`
	ShipID    string          `json:"ship_id"`
	Kind      string          `json:"kind"`
	Reason    string          `json:"reason"`
	Details   json.RawMessage `json:"details" gorm:"type:jsonb"`
	CreatedAt time.Time       `json:"created_at"`
}

func (Decision) TableName() string { return "decisions" }

type Event struct {
	ID        int64           `json:"-" gorm:"primaryKey"`
	AccountID string          `json:"account_id"`
	EventID   string          `json:"event_id"`
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload" gorm:"type:jsonb"`
	CreatedAt time.Time       `json:"created_at"`
}

func (Event) TableName() string { return "events" }

type RequestSummary struct {
	ID         int64     `json:"id" gorm:"primaryKey"`
	AccountID  string    `json:"account_id"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Status     int       `json:"status"`
	DurationMS int64     `json:"duration_ms"`
	Error      string    `json:"error"`
	CreatedAt  time.Time `json:"created_at"`
}

type Request struct {
	RequestSummary `gorm:"embedded"`
	Request        json.RawMessage `json:"request" gorm:"type:jsonb"`
	Response       json.RawMessage `json:"response" gorm:"type:jsonb"`
}

func (Request) TableName() string { return "requests" }

type DashboardAccount struct {
	Account
	Snapshot  map[string]json.RawMessage `json:"snapshot"`
	UpdatedAt *time.Time                 `json:"updated_at"`
}

type Dashboard struct {
	UpdatedAt   time.Time          `json:"updated_at"`
	Accounts    []DashboardAccount `json:"accounts"`
	Metrics     []Metric           `json:"metrics"`
	Decisions   []Decision         `json:"decisions"`
	Events      []Event            `json:"events"`
	Requests    []RequestSummary   `json:"requests"`
	Settlements []Settlement       `json:"settlements"`
}

type ServiceStats struct {
	Accounts        int64
	EnabledAccounts int64
	PendingActions  int64
	Counters        []Metric
	Metrics         []Metric
}

type accountSecret struct {
	AccountID  string `gorm:"primaryKey"`
	Ciphertext []byte
}

func (accountSecret) TableName() string { return "account_secrets" }

type snapshotRow struct {
	AccountID string          `gorm:"primaryKey"`
	Data      json.RawMessage `gorm:"type:jsonb"`
	UpdatedAt time.Time
}

func (snapshotRow) TableName() string { return "snapshots" }

type actionRow struct {
	ID             string `gorm:"primaryKey"`
	AccountID      string
	CompanyID      string
	Method         string
	Path           string
	BodyCipher     []byte
	State          string
	Status         int
	ResponseCipher []byte
	RetryAfter     *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (actionRow) TableName() string { return "actions" }

type cursorRow struct {
	AccountID     string `gorm:"primaryKey"`
	EventID       string
	EventSequence int64
	UpdatedAt     time.Time
}

func (cursorRow) TableName() string { return "event_cursors" }

type metricCounter struct {
	AccountID  string
	Name       string
	Value      float64
	RecordedAt time.Time
}

func (metricCounter) TableName() string { return "metric_counters" }
