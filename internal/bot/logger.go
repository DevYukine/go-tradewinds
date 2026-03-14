package bot

import (
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/DevYukine/go-tradewinds/internal/db"
)

// LogEntry represents a single log line with metadata.
type LogEntry struct {
	Level     string
	Message   string
	CreatedAt time.Time
}

// CompanyLogger wraps a zap logger to also persist log lines to the database
// and broadcast them to live subscribers (for dashboard SSE streaming).
type CompanyLogger struct {
	zap         *zap.Logger
	companyDBID uint
	db          *gorm.DB

	ringBuffer []LogEntry
	bufferSize int
	bufferPos  int
	bufferFull bool

	subscribers map[int]chan LogEntry
	nextSubID   int
	mu          sync.RWMutex
}

const defaultBufferSize = 200

// NewCompanyLogger creates a logger that persists to DB and supports live subscribers.
func NewCompanyLogger(base *zap.Logger, companyDBID uint, gormDB *gorm.DB) *CompanyLogger {
	return &CompanyLogger{
		zap:         base,
		companyDBID: companyDBID,
		db:          gormDB,
		ringBuffer:  make([]LogEntry, defaultBufferSize),
		bufferSize:  defaultBufferSize,
		subscribers: make(map[int]chan LogEntry),
	}
}

// Info logs at info level.
func (cl *CompanyLogger) Info(msg string, fields ...zap.Field) {
	cl.zap.Info(msg, fields...)
	cl.record("info", msg)
}

// Warn logs at warn level.
func (cl *CompanyLogger) Warn(msg string, fields ...zap.Field) {
	cl.zap.Warn(msg, fields...)
	cl.record("warn", msg)
}

// Error logs at error level.
func (cl *CompanyLogger) Error(msg string, fields ...zap.Field) {
	cl.zap.Error(msg, fields...)
	cl.record("error", msg)
}

// Debug logs at debug level (not persisted to DB to reduce noise).
func (cl *CompanyLogger) Debug(msg string, fields ...zap.Field) {
	cl.zap.Debug(msg, fields...)
}

// Trade logs a trade event.
func (cl *CompanyLogger) Trade(msg string, fields ...zap.Field) {
	cl.zap.Info(msg, fields...)
	cl.record("trade", msg)
}

// Event logs a game event.
func (cl *CompanyLogger) Event(msg string, fields ...zap.Field) {
	cl.zap.Info(msg, fields...)
	cl.record("event", msg)
}

// Agent logs an agent decision.
func (cl *CompanyLogger) Agent(msg string, fields ...zap.Field) {
	cl.zap.Info(msg, fields...)
	cl.record("agent", msg)
}

// Subscribe returns a channel that receives new log entries in real-time.
// Call Unsubscribe with the returned ID when done.
func (cl *CompanyLogger) Subscribe() (int, <-chan LogEntry) {
	cl.mu.Lock()
	defer cl.mu.Unlock()

	ch := make(chan LogEntry, 50)
	id := cl.nextSubID
	cl.nextSubID++
	cl.subscribers[id] = ch
	return id, ch
}

// Unsubscribe removes a live log subscriber.
func (cl *CompanyLogger) Unsubscribe(id int) {
	cl.mu.Lock()
	defer cl.mu.Unlock()

	if ch, ok := cl.subscribers[id]; ok {
		close(ch)
		delete(cl.subscribers, id)
	}
}

// RecentLogs returns the most recent log entries from the ring buffer.
func (cl *CompanyLogger) RecentLogs(n int) []LogEntry {
	cl.mu.RLock()
	defer cl.mu.RUnlock()

	if n > cl.bufferSize {
		n = cl.bufferSize
	}

	var count int
	if cl.bufferFull {
		count = cl.bufferSize
	} else {
		count = cl.bufferPos
	}

	if n > count {
		n = count
	}

	result := make([]LogEntry, n)
	for i := range n {
		idx := (cl.bufferPos - n + i + cl.bufferSize) % cl.bufferSize
		result[i] = cl.ringBuffer[idx]
	}
	return result
}

// record persists a log entry to the ring buffer, database, and subscribers.
func (cl *CompanyLogger) record(level, msg string) {
	entry := LogEntry{
		Level:     level,
		Message:   msg,
		CreatedAt: time.Now(),
	}

	cl.mu.Lock()
	// Ring buffer.
	cl.ringBuffer[cl.bufferPos] = entry
	cl.bufferPos = (cl.bufferPos + 1) % cl.bufferSize
	if cl.bufferPos == 0 {
		cl.bufferFull = true
	}

	// Broadcast to subscribers (non-blocking).
	for _, ch := range cl.subscribers {
		select {
		case ch <- entry:
		default:
		}
	}
	cl.mu.Unlock()

	// Persist to DB asynchronously.
	go cl.persistToDB(level, msg)
}

// persistToDB writes a log entry to the database.
func (cl *CompanyLogger) persistToDB(level, msg string) {
	logEntry := db.CompanyLog{
		CompanyID: cl.companyDBID,
		Level:     level,
		Message:   msg,
	}

	if err := cl.db.Create(&logEntry).Error; err != nil {
		cl.zap.Error("failed to persist company log", zap.Error(err))
	}
}
