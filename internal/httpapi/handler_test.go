package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
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
		{name: "metrics unavailable", method: "GET", path: "/metrics", status: 503, body: "tradewinds_database_up 0"},
		{name: "mutation denied", method: "POST", path: "/api/dashboard", status: 405, body: `"error":"method_not_allowed"`},
		{name: "private journal denied", method: "GET", path: "/api/history?kind=actions", status: 400, body: `"error":"invalid_kind"`},
		{name: "missing kind", method: "GET", path: "/api/history", status: 400, body: `"error":"invalid_kind"`},
		{name: "large history denied", method: "GET", path: "/api/history?kind=events&limit=501", status: 400, body: `"error":"invalid_limit"`},
		{name: "zero history denied", method: "GET", path: "/api/history?kind=events&limit=0", status: 400, body: `"error":"invalid_limit"`},
		{name: "noninteger history denied", method: "GET", path: "/api/history?kind=events&limit=lots", status: 400, body: `"error":"invalid_limit"`},
		{name: "valid history needs database", method: "GET", path: "/api/history?kind=events&limit=500", status: 503, body: `"error":"database_unavailable"`},
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
