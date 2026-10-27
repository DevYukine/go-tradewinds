package gameapi_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"go.uber.org/zap"
)

func TestReleaseNotesPublicEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/update" {
			t.Errorf("request = %s %s, want GET /update", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("public release notes request must work without an Authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[
			{"version":"0.4.1","released_on":"2026-10-27","markdown":"# Improvements\n\nRelease details."},
			{"version":"0.4.0","released_on":"2026-10-26","markdown":"# R&D"}
		]`)
	}))
	defer server.Close()

	client := gameapi.New(server.URL, "", zap.NewNop())
	got, err := client.ReleaseNotes(context.Background())
	if err != nil {
		t.Fatalf("ReleaseNotes without credentials: %v", err)
	}
	want := []gameapi.ReleaseNote{
		{Version: "0.4.1", ReleasedOn: "2026-10-27", Markdown: "# Improvements\n\nRelease details."},
		{Version: "0.4.0", ReleasedOn: "2026-10-26", Markdown: "# R&D"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("release notes = %#v, want %#v", got, want)
	}
}
