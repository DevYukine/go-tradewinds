package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestReadOnlyRoutesAndTruthfulUnavailableResponses(t *testing.T) {
	handler := NewHandler(nil, zap.NewNop(), "127.0.0.1:8585")
	tests := []struct {
		name   string
		method string
		path   string
		status int
		body   string
	}{
		{name: "live process", method: "GET", path: "/healthz", status: 200, body: `"status":"ok"`},
		{name: "database unavailable", method: "GET", path: "/readyz", status: 503, body: `"error":"database_unavailable"`},
		{name: "dashboard unavailable", method: "GET", path: "/api/dashboard", status: 503, body: `"error":"database_unavailable"`},
		{name: "competition unavailable", method: "GET", path: "/api/competition?account_id=one", status: 503, body: `"error":"database_unavailable"`},
		{name: "competition scope required", method: "GET", path: "/api/competition", status: 400, body: `"error":"invalid_account"`},
		{name: "competition blank scope", method: "GET", path: "/api/competition?account_id=%20", status: 400, body: `"error":"invalid_account"`},
		{name: "competition oversized scope", method: "GET", path: "/api/competition?account_id=" + strings.Repeat("a", 256), status: 400, body: `"error":"invalid_account"`},
		{name: "competition mutation denied", method: "POST", path: "/api/competition?account_id=one", status: 405, body: `"error":"method_not_allowed"`},
		{name: "metrics unavailable", method: "GET", path: "/metrics", status: 503, body: "tradewinds_database_up 0"},
		{name: "mutation denied", method: "POST", path: "/api/dashboard", status: 405, body: `"error":"method_not_allowed"`},
		{name: "private journal denied", method: "GET", path: "/api/history?kind=actions", status: 400, body: `"error":"invalid_kind"`},
		{name: "missing kind", method: "GET", path: "/api/history", status: 400, body: `"error":"invalid_kind"`},
		{name: "large history denied", method: "GET", path: "/api/history?kind=events&limit=501", status: 400, body: `"error":"invalid_limit"`},
		{name: "zero history denied", method: "GET", path: "/api/history?kind=events&limit=0", status: 400, body: `"error":"invalid_limit"`},
		{name: "noninteger history denied", method: "GET", path: "/api/history?kind=events&limit=lots", status: 400, body: `"error":"invalid_limit"`},
		{name: "valid history needs database", method: "GET", path: "/api/history?kind=events&limit=500", status: 503, body: `"error":"database_unavailable"`},
		{name: "money history unavailable", method: "GET", path: "/api/money-history?account_id=one&range=all", status: 503, body: `"error":"database_unavailable"`},
		{name: "money scope required", method: "GET", path: "/api/money-history?range=all", status: 400, body: `"error":"invalid_account"`},
		{name: "money range required", method: "GET", path: "/api/money-history?account_id=one", status: 400, body: `"error":"invalid_range"`},
		{name: "money range rejected", method: "GET", path: "/api/money-history?account_id=one&range=year", status: 400, body: `"error":"invalid_range"`},
		{name: "money custom dates required", method: "GET", path: "/api/money-history?account_id=one&range=custom", status: 400, body: `"error":"invalid_range"`},
		{name: "money zero custom start rejected", method: "GET", path: "/api/money-history?account_id=one&range=custom&from=0001-01-01T00:00:00Z&to=2026-10-25T12:00:00Z", status: 400, body: `"error":"invalid_range"`},
		{name: "money reversed range rejected", method: "GET", path: "/api/money-history?account_id=one&range=custom&from=2026-10-25T12:00:00Z&to=2026-10-24T12:00:00Z", status: 400, body: `"error":"invalid_range"`},
		{name: "money custom dates respected", method: "GET", path: "/api/money-history?account_id=one&range=custom&from=2026-10-24T12:00:00%2B02:00&to=2026-10-25T12:00:00Z", status: 503, body: `"error":"database_unavailable"`},
		{name: "money dates need custom mode", method: "GET", path: "/api/money-history?account_id=one&range=24h&from=2026-10-24T12:00:00Z", status: 400, body: `"error":"invalid_range"`},
		{name: "standings all unavailable", method: "GET", path: "/api/standings-history?account_id=one&range=all", status: 503, body: `"error":"database_unavailable"`},
		{name: "standings 24h unavailable", method: "GET", path: "/api/standings-history?account_id=one&range=24h", status: 503, body: `"error":"database_unavailable"`},
		{name: "standings 7d unavailable", method: "GET", path: "/api/standings-history?account_id=one&range=7d", status: 503, body: `"error":"database_unavailable"`},
		{name: "standings 30d unavailable", method: "GET", path: "/api/standings-history?account_id=one&range=30d&top=20", status: 503, body: `"error":"database_unavailable"`},
		{name: "standings scope required", method: "GET", path: "/api/standings-history?range=all", status: 400, body: `"error":"invalid_account"`},
		{name: "standings range required", method: "GET", path: "/api/standings-history?account_id=one", status: 400, body: `"error":"invalid_range"`},
		{name: "standings range invalid", method: "GET", path: "/api/standings-history?account_id=one&range=year", status: 400, body: `"error":"invalid_range"`},
		{name: "standings overrides rejected", method: "GET", path: "/api/standings-history?account_id=one&range=all&from=2026-10-24", status: 400, body: `"error":"invalid_range"`},
		{name: "standings top bounded", method: "GET", path: "/api/standings-history?account_id=one&range=all&top=21", status: 400, body: `"error":"invalid_top"`},
		{name: "standings top nonzero", method: "GET", path: "/api/standings-history?account_id=one&range=all&top=0", status: 400, body: `"error":"invalid_top"`},
		{name: "standings mutation denied", method: "POST", path: "/api/standings-history?account_id=one&range=all", status: 405, body: `"error":"method_not_allowed"`},
		{name: "unknown route", method: "GET", path: "/api/command", status: 404, body: `"error":"not_found"`},
		{name: "oversized URI", method: "GET", path: "/api/history?kind=events&search=" + strings.Repeat("x", 4096), status: 414, body: `"error":"uri_too_long"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			request.Host = "127.0.0.1:8585"
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status || !strings.Contains(response.Body.String(), test.body) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("read responses permit caching or content sniffing")
			}
			if response.Code == http.StatusMethodNotAllowed && response.Header().Get("Allow") != "GET" {
				t.Fatal("read-only API did not advertise its only permitted method")
			}
		})
	}
}

func TestForeignHostRejected(t *testing.T) {
	handler := NewHandler(nil, zap.NewNop(), "127.0.0.1:8585")
	request := httptest.NewRequest(http.MethodGet, "http://rebind.attacker.example:8585/api/dashboard", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"error":"host_not_allowed"`) {
		t.Fatalf("foreign Host accepted: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestMetricLabelsEscapePrometheusDelimiters(t *testing.T) {
	if got := metricLabel("account\\one\n\"two"); got != `"account\\one\n\"two"` {
		t.Fatalf("unsafe metric label: %s", got)
	}
}

type disconnectedResponseWriter struct {
	*httptest.ResponseRecorder
	err   error
	calls int
}

func (w *disconnectedResponseWriter) Write([]byte) (int, error) {
	w.calls++
	return 0, w.err
}

func TestRepeatedResponseWriteFailuresAreBounded(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "disconnect", err: errors.New("client disconnected")},
		{name: "short write"},
	} {
		t.Run(test.name, func(t *testing.T) {
			core, logs := observer.New(zap.DebugLevel)
			handler := NewHandler(nil, zap.New(core), "127.0.0.1:8585")
			response := &disconnectedResponseWriter{
				ResponseRecorder: httptest.NewRecorder(),
				err:              test.err,
			}
			for range 50 {
				request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
				request.Host = "127.0.0.1:8585"
				handler.ServeHTTP(response, request)
			}
			if response.calls != 50 {
				t.Fatalf("sampling prevented response writes: %d", response.calls)
			}
			entries := logs.All()
			if len(entries) != 1 || entries[0].Level != zap.WarnLevel {
				t.Fatalf("repeated failed writes were not bounded to one warning: %+v", entries)
			}
			fields := entries[0].ContextMap()
			if fields["written_bytes"] != int64(0) || fields["expected_bytes"] == int64(0) {
				t.Fatalf("write warning lacks byte-count context: %+v", fields)
			}
			if test.err != nil && fields["error"] != test.err.Error() {
				t.Fatalf("write warning discarded its cause: %+v", fields)
			}
		})
	}
}
