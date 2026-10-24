package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const maxResponseBytes = 32 * 1024 * 1024

type handler struct {
	store       *store.Store
	log         *zap.Logger
	readLog     *zap.Logger
	allowedHost map[string]bool
}

// NewHandler builds the read-only dashboard API. addr is the configured
// listener address; requests whose Host header does not name that endpoint
// are refused so a DNS-rebinding page cannot read loopback responses
// same-origin in browsers that do not enforce Private Network Access.
func NewHandler(db *store.Store, log *zap.Logger, addr string) http.Handler {
	if log == nil {
		log = zap.NewNop()
	}
	allowed := map[string]bool{}
	host, port, err := net.SplitHostPort(addr)
	if err == nil {
		if host == "" || host == "127.0.0.1" || host == "localhost" {
			allowed[net.JoinHostPort("127.0.0.1", port)] = true
			allowed[net.JoinHostPort("localhost", port)] = true
			allowed[net.JoinHostPort("::1", port)] = true
			allowed[net.JoinHostPort("[::1]", port)] = true
		} else {
			allowed[net.JoinHostPort(host, port)] = true
		}
	}
	// Dashboard polls can fail together during an outage or client disconnects.
	// Bound identical read/write warnings to one per minute, even at debug.
	readLog := log.WithOptions(zap.WrapCore(func(core zapcore.Core) zapcore.Core {
		return zapcore.NewSamplerWithOptions(core, time.Minute, 1, 0)
	}))
	return &handler{store: db, log: log, readLog: readLog, allowedHost: allowed}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !h.allowedHost[r.Host] {
		h.failure(w, http.StatusForbidden, "host_not_allowed", "This API answers only the configured dashboard listener.")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		h.failure(w, http.StatusMethodNotAllowed, "method_not_allowed", "This API is read-only; use GET.")
		return
	}
	if len(r.URL.RequestURI()) > 4096 {
		h.failure(w, http.StatusRequestURITooLong, "uri_too_long", "The request URI is too long.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	switch r.URL.Path {
	case "/healthz":
		h.json(w, http.StatusOK, map[string]string{"status": "ok"})
	case "/readyz":
		if h.store == nil || h.store.Ping(ctx) != nil {
			h.failure(w, http.StatusServiceUnavailable, "database_unavailable", "PostgreSQL is not ready.")
			return
		}
		h.json(w, http.StatusOK, map[string]string{"status": "ready"})
	case "/api/dashboard":
		h.dashboard(ctx, w)
	case "/api/history":
		h.history(ctx, w, r)
	case "/metrics":
		h.metrics(ctx, w)
	default:
		h.failure(w, http.StatusNotFound, "not_found", "No such read endpoint.")
	}
}

func (h *handler) dashboard(ctx context.Context, w http.ResponseWriter) {
	if h.store == nil {
		h.failure(w, http.StatusServiceUnavailable, "database_unavailable", "PostgreSQL is not ready.")
		return
	}
	data, err := h.store.Dashboard(ctx)
	if err != nil {
		h.readError(w, "dashboard", err)
		return
	}
	h.json(w, http.StatusOK, data)
}

func (h *handler) history(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	kind := query.Get("kind")
	switch kind {
	case "events", "requests", "decisions", "metrics", "settlements", "journal":
	default:
		h.failure(w, http.StatusBadRequest, "invalid_kind", "kind must be events, requests, decisions, metrics, settlements, or journal.")
		return
	}
	limit := 100
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > store.MaxHistoryLimit {
			h.failure(w, http.StatusBadRequest, "invalid_limit", "limit must be an integer from 1 through 500.")
			return
		}
		limit = parsed
	}
	accountID := query.Get("account_id")
	if len(accountID) > 255 || accountID != "" && strings.TrimSpace(accountID) == "" || kind == "journal" && accountID == "" {
		h.failure(w, http.StatusBadRequest, "invalid_account", "account_id must be valid; journal history requires it.")
		return
	}
	if h.store == nil {
		h.failure(w, http.StatusServiceUnavailable, "database_unavailable", "PostgreSQL is not ready.")
		return
	}
	data, err := h.store.History(ctx, accountID, kind, limit)
	if err != nil {
		h.readError(w, "history", err)
		return
	}
	h.json(w, http.StatusOK, data)
}

func (h *handler) metrics(ctx context.Context, w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if h.store == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		h.write(w, []byte("# TYPE tradewinds_database_up gauge\ntradewinds_database_up 0\n"))
		return
	}
	stats, err := h.store.ServiceStats(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		h.readLog.Warn("read metrics failed", zap.Error(err))
		databaseUp := 1
		if h.store.Ping(ctx) != nil {
			databaseUp = 0
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		h.write(w, []byte(fmt.Sprintf(
			"# TYPE tradewinds_database_up gauge\ntradewinds_database_up %d\n# TYPE tradewinds_metrics_read_up gauge\ntradewinds_metrics_read_up 0\n",
			databaseUp,
		)))
		return
	}
	var text strings.Builder
	text.WriteString("# TYPE tradewinds_database_up gauge\ntradewinds_database_up 1\n")
	text.WriteString("# TYPE tradewinds_metrics_read_up gauge\ntradewinds_metrics_read_up 1\n")
	fmt.Fprintf(&text, "# TYPE tradewinds_accounts gauge\ntradewinds_accounts %d\n", stats.Accounts)
	fmt.Fprintf(&text, "# TYPE tradewinds_enabled_accounts gauge\ntradewinds_enabled_accounts %d\n", stats.EnabledAccounts)
	fmt.Fprintf(&text, "# TYPE tradewinds_pending_actions gauge\ntradewinds_pending_actions %d\n", stats.PendingActions)
	text.WriteString("# TYPE tradewinds_http_requests_total counter\n")
	text.WriteString("# TYPE tradewinds_http_request_errors_total counter\n")
	text.WriteString("# TYPE tradewinds_http_request_duration_seconds_total counter\n")
	for _, counter := range stats.Counters {
		name := "tradewinds_" + counter.Name
		value := counter.Value
		if counter.Name == "http_request_duration_ms_total" {
			name = "tradewinds_http_request_duration_seconds_total"
			value /= 1000
		}
		fmt.Fprintf(&text, "%s{account_id=%s} %s\n", name, metricLabel(counter.AccountID), strconv.FormatFloat(value, 'g', -1, 64))
	}
	text.WriteString("# TYPE tradewinds_account_metric gauge\n")
	text.WriteString("# TYPE tradewinds_account_metric_recorded_timestamp_seconds gauge\n")
	for _, metric := range stats.Metrics {
		labels := "account_id=" + metricLabel(metric.AccountID) + ",name=" + metricLabel(metric.Name)
		fmt.Fprintf(&text, "tradewinds_account_metric{%s} %s\n", labels, strconv.FormatFloat(metric.Value, 'g', -1, 64))
		fmt.Fprintf(&text, "tradewinds_account_metric_recorded_timestamp_seconds{%s} %d\n", labels, metric.RecordedAt.Unix())
	}
	h.write(w, []byte(text.String()))
}

func metricLabel(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\n", "\\n")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	return "\"" + value + "\""
}

func (h *handler) readError(w http.ResponseWriter, operation string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	h.readLog.Warn("read "+operation+" failed", zap.Error(err))
	switch {
	case errors.Is(err, store.ErrInvalidInput):
		h.failure(w, http.StatusBadRequest, "invalid_query", "The read query is invalid.")
	case errors.Is(err, store.ErrNotFound):
		h.failure(w, http.StatusNotFound, "not_found", "No such stored record.")
	case errors.Is(err, store.ErrResponseTooLarge):
		h.failure(w, http.StatusServiceUnavailable, "response_too_large", "Stored data exceeds this bounded endpoint; request a smaller history window.")
	default:
		h.failure(w, http.StatusServiceUnavailable, "database_unavailable", "The database read failed; no empty success was substituted.")
	}
}

func (h *handler) failure(w http.ResponseWriter, status int, code, detail string) {
	h.json(w, status, struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}{Error: code, Detail: detail})
}

func (h *handler) json(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		h.log.Error("encode read response", zap.Error(err))
		status = http.StatusInternalServerError
		body = []byte(`{"error":"encoding_failed","detail":"Stored data could not be encoded."}`)
	} else if len(body) > maxResponseBytes {
		status = http.StatusUnprocessableEntity
		body = []byte(`{"error":"response_too_large","detail":"The requested result exceeds 32 MiB; reduce the history limit."}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	h.write(w, body)
}

func (h *handler) write(w http.ResponseWriter, body []byte) {
	n, err := w.Write(body)
	if err != nil || n != len(body) {
		h.readLog.Warn("write read response", zap.Error(err), zap.Int("written_bytes", n), zap.Int("expected_bytes", len(body)))
	}
}
