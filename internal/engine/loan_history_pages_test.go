package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func loanHistoryPageFixture(start, count int) []json.RawMessage {
	rows := make([]json.RawMessage, count)
	for index := range rows {
		rows[index] = json.RawMessage(fmt.Sprintf(`{"id":"history-%04d","status":"repaid"}`, start+index))
	}
	return rows
}

func loanHistoryPaddedFixture(id, padding string) json.RawMessage {
	return json.RawMessage(`{"id":"` + id + `","padding":"` + padding + `"}`)
}

// Write the wire rows without JSON HTML escaping so the client, rather than the
// fixture encoder, encounters any expansion charged by the capture budget.
func writeLoanHistoryPage(writer http.ResponseWriter, rows []json.RawMessage) {
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write([]byte("["))
	for index, row := range rows {
		if index > 0 {
			_, _ = writer.Write([]byte(","))
		}
		_, _ = writer.Write(row)
	}
	_, _ = writer.Write([]byte("]"))
}

func requireLoanHistoryRows(t *testing.T, raw json.RawMessage, want []json.RawMessage) {
	t.Helper()
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, encoded) {
		t.Fatalf("captured history differs from all received rows: got %d bytes, want %d", len(raw), len(encoded))
	}
}

func TestReadLoanHistoryPagesCombinesOrderedPages(t *testing.T) {
	pages := [][]json.RawMessage{
		loanHistoryPageFixture(0, 200),
		loanHistoryPageFixture(200, 200),
		loanHistoryPageFixture(400, 3),
	}
	cursors := []string{"", "history-0199", "history-0399"}
	var calls atomic.Int32
	runner := newRetentionRunner(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		index := int(calls.Add(1)) - 1
		query := request.URL.Query()
		if index >= len(pages) {
			t.Errorf("unexpected history page %d", index+1)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Method != http.MethodGet || request.URL.Path != "/v1/loan/list" ||
			query.Get("role") != "borrower" || query.Get("limit") != "200" || query.Get("status") != "" ||
			query.Get("cursor") != cursors[index] {
			t.Errorf("incorrect borrower history request: %s %s", request.Method, request.URL)
		}
		wantParameters := 2
		if index > 0 {
			wantParameters++
		}
		if len(query) != wantParameters {
			t.Errorf("unexpected history filters: %s", request.URL)
		}
		writeLoanHistoryPage(writer, pages[index])
	}))

	raw, err := readLoanHistoryPages(context.Background(), runner.api)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]json.RawMessage, 0, 403)
	for _, page := range pages {
		want = append(want, page...)
	}
	requireLoanHistoryRows(t, raw, want)
	if calls.Load() != 3 {
		t.Fatalf("requested %d pages, want 3", calls.Load())
	}
}

func TestReadLoanHistoryPagesBoundsPageRequests(t *testing.T) {
	for _, complete := range []bool{true, false} {
		name := "full twentieth page fails closed"
		if complete {
			name = "short twentieth page completes"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			runner := newRetentionRunner(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				index := int(calls.Add(1)) - 1
				if index >= 20 {
					t.Error("history requested a twenty-first page")
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				wantCursor := ""
				if index > 0 {
					wantCursor = fmt.Sprintf("history-%04d", index*200-1)
				}
				if request.URL.Query().Get("cursor") != wantCursor {
					t.Errorf("page %d has incorrect cursor: %s", index+1, request.URL)
				}
				count := 200
				if complete && index == 19 {
					count = 199
				}
				writeLoanHistoryPage(writer, loanHistoryPageFixture(index*200, count))
			}))

			raw, err := readLoanHistoryPages(context.Background(), runner.api)
			if complete {
				if err != nil {
					t.Fatal(err)
				}
				requireLoanHistoryRows(t, raw, loanHistoryPageFixture(0, 3999))
			} else if !errors.Is(err, store.ErrResponseTooLarge) || raw != nil || !strings.Contains(err.Error(), "20-page") {
				t.Fatalf("full history at the page cap returned partial data or wrong error: bytes=%d, err=%v", len(raw), err)
			}
			if calls.Load() != 20 {
				t.Fatalf("requested %d pages, want exactly 20", calls.Load())
			}
		})
	}
}

func TestReadLoanHistoryPagesRejectsOversizedPagesAndRowBudgets(t *testing.T) {
	for _, test := range []struct {
		name       string
		count      int
		malformed  bool
		priorPages int
		wantError  string
	}{
		{name: "oversized page", count: 201, wantError: "200-row limit"},
		{name: "oversized malformed page", count: 201, malformed: true, wantError: "200-row limit"},
		{name: "oversized page after good page", count: 201, priorPages: 1, wantError: "200-row limit"},
		{name: "many malformed rows", count: 4001, malformed: true, wantError: "200-row limit"},
		{name: "row budget", count: 201, priorPages: 19, wantError: "4000-row capture budget"},
		{name: "malformed rows consume row budget", count: 201, malformed: true, priorPages: 19, wantError: "4000-row capture budget"},
	} {
		t.Run(test.name, func(t *testing.T) {
			page := loanHistoryPageFixture(test.priorPages*200, test.count)
			if test.malformed {
				for index := range page {
					page[index] = json.RawMessage(`null`)
				}
			}
			var calls atomic.Int32
			runner := newRetentionRunner(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				call := int(calls.Add(1))
				if call <= test.priorPages {
					writeLoanHistoryPage(writer, loanHistoryPageFixture((call-1)*200, 200))
					return
				}
				writeLoanHistoryPage(writer, page)
			}))

			raw, err := readLoanHistoryPages(context.Background(), runner.api)
			if !errors.Is(err, store.ErrResponseTooLarge) || raw != nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("oversized history returned partial data or wrong error: bytes=%d, err=%v", len(raw), err)
			}
			wantCalls := int32(test.priorPages + 1)
			if calls.Load() != wantCalls {
				t.Fatalf("requested %d pages after oversized response, want %d", calls.Load(), wantCalls)
			}
		})
	}
}

func TestReadLoanHistoryPagesBoundsAggregateBytesIncludingSeparators(t *testing.T) {
	for _, excess := range []int{0, 1} {
		name := "exactly at encoded budget"
		if excess > 0 {
			name = "malformed tail exceeds budget by one byte"
		}
		t.Run(name, func(t *testing.T) {
			pages := [][]json.RawMessage{
				loanHistoryPageFixture(0, 200),
				loanHistoryPageFixture(200, 200),
				{loanHistoryPaddedFixture("history-0400", "")},
			}
			padding := strings.Repeat("x", 6<<20)
			pages[0][0] = loanHistoryPaddedFixture("history-0000", padding)
			pages[1][0] = loanHistoryPaddedFixture("history-0200", padding)
			if excess > 0 {
				pages[2][0] = json.RawMessage(`""`)
			}
			rowBytes := 0
			for _, page := range pages {
				for _, row := range page {
					rowBytes += len(row)
				}
			}
			// There are 401 rows, 400 commas, and two brackets. The overflow
			// fixture's row bytes alone still fit; only the encoded list exceeds.
			tailPadding := store.MaxSnapshotBytes + excess - rowBytes - 402
			if tailPadding <= 0 {
				t.Fatal("fixture does not leave room for its final page")
			}
			if excess > 0 {
				pages[2][0] = json.RawMessage(`"` + strings.Repeat("x", tailPadding) + `"`)
			} else {
				pages[2][0] = loanHistoryPaddedFixture("history-0400", strings.Repeat("x", tailPadding))
			}
			var calls atomic.Int32
			runner := newRetentionRunner(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				index := int(calls.Add(1)) - 1
				if index >= len(pages) {
					t.Error("history requested a page after its complete or oversized tail")
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				writeLoanHistoryPage(writer, pages[index])
			}))

			raw, err := readLoanHistoryPages(context.Background(), runner.api)
			if excess == 0 {
				if err != nil || len(raw) != store.MaxSnapshotBytes {
					t.Fatalf("exact-budget complete history was rejected: bytes=%d, err=%v", len(raw), err)
				}
				var rows []json.RawMessage
				if err := json.Unmarshal(raw, &rows); err != nil || len(rows) != 401 {
					t.Fatalf("exact-budget history lost rows: count=%d, err=%v", len(rows), err)
				}
			} else if !errors.Is(err, store.ErrResponseTooLarge) || raw != nil {
				t.Fatalf("oversized malformed tail returned partial data: bytes=%d, err=%v", len(raw), err)
			}
			if calls.Load() != 3 {
				t.Fatalf("requested %d pages, want 3", calls.Load())
			}
		})
	}
}

func TestReadLoanHistoryPagesChargesMarshalExpansionBeforeAccumulation(t *testing.T) {
	for _, test := range []struct {
		name      string
		unit      string
		wantCalls int32
	}{
		{name: "HTML escapes", unit: "<>&", wantCalls: 2},
		{name: "Unicode line separators", unit: "\u2028\u2029", wantCalls: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			padding := strings.Repeat(test.unit, 512*1024)
			var calls atomic.Int32
			runner := newRetentionRunner(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				index := int(calls.Add(1)) - 1
				if index >= 3 {
					writeLoanHistoryPage(writer, []json.RawMessage{})
					return
				}
				page := loanHistoryPageFixture(index*200, 200)
				page[0] = loanHistoryPaddedFixture(fmt.Sprintf("history-%04d", index*200), padding)
				writeLoanHistoryPage(writer, page)
			}))

			raw, err := readLoanHistoryPages(context.Background(), runner.api)
			if !errors.Is(err, store.ErrResponseTooLarge) || raw != nil {
				t.Fatalf("encoded expansion exceeded the budget without rejection: bytes=%d, err=%v", len(raw), err)
			}
			if calls.Load() != test.wantCalls {
				t.Fatalf("requested %d pages before rejecting encoded expansion, want %d", calls.Load(), test.wantCalls)
			}
		})
	}
}

func TestReadLoanHistoryPagesRejectsDuplicateIDsAndUnusableCursors(t *testing.T) {
	for _, test := range []struct {
		name      string
		wantError string
	}{
		{name: "duplicate within complete page", wantError: "repeated an id or cursor"},
		{name: "duplicate across pages", wantError: "repeated an id or cursor"},
		{name: "repeated cursor", wantError: "repeated an id or cursor"},
		{name: "full page without usable cursor", wantError: "no usable cursor"},
		{name: "later full page without usable cursor", wantError: "no usable cursor"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var pages [][]json.RawMessage
			switch test.name {
			case "duplicate within complete page":
				page := loanHistoryPageFixture(0, 2)
				page[1] = page[0]
				pages = [][]json.RawMessage{page}
			case "duplicate across pages":
				first := loanHistoryPageFixture(0, 200)
				pages = [][]json.RawMessage{first, {first[50]}}
			case "repeated cursor":
				first, second := loanHistoryPageFixture(0, 200), loanHistoryPageFixture(200, 200)
				second[199] = first[199]
				pages = [][]json.RawMessage{first, second}
			default:
				page := make([]json.RawMessage, 200)
				for index := range page {
					page[index] = json.RawMessage(`{"id":7,"status":"repaid"}`)
				}
				if test.name == "later full page without usable cursor" {
					pages = append(pages, loanHistoryPageFixture(0, 200))
				}
				pages = append(pages, page)
			}
			var calls atomic.Int32
			runner := newRetentionRunner(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				index := int(calls.Add(1)) - 1
				if index >= len(pages) {
					t.Error("history continued after an invalid cursor")
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				writeLoanHistoryPage(writer, pages[index])
			}))

			raw, err := readLoanHistoryPages(context.Background(), runner.api)
			if err == nil || raw != nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("invalid history pagination returned partial data or wrong error: bytes=%d, err=%v", len(raw), err)
			}
			if int(calls.Load()) != len(pages) {
				t.Fatalf("requested %d pages, want %d", calls.Load(), len(pages))
			}
		})
	}
}

func TestReadLoanHistoryPagesRetainsMalformedRowsAndUsesLastValidID(t *testing.T) {
	for _, name := range []string{
		"empty complete page",
		"mixed malformed complete page",
		"only malformed complete page",
		"invalid ID at full page end",
		"invalid status at full page end",
	} {
		t.Run(name, func(t *testing.T) {
			var pages [][]json.RawMessage
			cursors := []string{""}
			switch name {
			case "empty complete page":
				pages = [][]json.RawMessage{{}}
			case "mixed malformed complete page":
				pages = [][]json.RawMessage{{
					json.RawMessage(`{"id":"good","status":"repaid"}`),
					json.RawMessage(`null`),
					json.RawMessage(`{"id":7,"status":"repaid"}`),
					json.RawMessage(`{"status":"repaid"}`),
					json.RawMessage(`{"id":"bad-status","status":{"unexpected":true}}`),
					json.RawMessage(`{"id":"unknown-status","status":"not-a-loan-status"}`),
					json.RawMessage(`"not a loan"`),
					json.RawMessage(`[]`),
				}}
			case "only malformed complete page":
				pages = [][]json.RawMessage{{json.RawMessage(`null`), json.RawMessage(`null`), json.RawMessage(`7`), json.RawMessage(`{}`)}}
			default:
				page := loanHistoryPageFixture(0, 200)
				cursor := "history-0198"
				page[199] = json.RawMessage(`{"id":7,"status":"repaid"}`)
				if name == "invalid status at full page end" {
					cursor = "history-0199"
					page[199] = json.RawMessage(`{"id":"history-0199","status":false}`)
				}
				pages = [][]json.RawMessage{page, {json.RawMessage(`{"status":"repaid"}`), json.RawMessage(`null`)}}
				cursors = append(cursors, cursor)
			}
			var calls atomic.Int32
			runner := newRetentionRunner(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				index := int(calls.Add(1)) - 1
				if index >= len(pages) {
					t.Error("history continued after a complete malformed page")
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				if request.URL.Query().Get("cursor") != cursors[index] {
					t.Errorf("did not use the last valid history ID: %s", request.URL)
				}
				writeLoanHistoryPage(writer, pages[index])
			}))

			raw, err := readLoanHistoryPages(context.Background(), runner.api)
			if err != nil {
				t.Fatal(err)
			}
			want := make([]json.RawMessage, 0)
			for _, page := range pages {
				want = append(want, page...)
			}
			requireLoanHistoryRows(t, raw, want)
			if int(calls.Load()) != len(pages) {
				t.Fatalf("requested %d pages, want %d", calls.Load(), len(pages))
			}
		})
	}
}

func TestReadLoanHistoryPagesCancellationDiscardsPartialHistory(t *testing.T) {
	t.Run("already canceled", func(t *testing.T) {
		var calls atomic.Int32
		runner := newRetentionRunner(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			calls.Add(1)
			writeLoanHistoryPage(writer, []json.RawMessage{})
		}))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		raw, err := readLoanHistoryPages(ctx, runner.api)
		if !errors.Is(err, context.Canceled) || raw != nil || calls.Load() != 0 {
			t.Fatalf("canceled history performed work or returned data: bytes=%d, calls=%d, err=%v", len(raw), calls.Load(), err)
		}
	})

	t.Run("between pages", func(t *testing.T) {
		var calls atomic.Int32
		runner := newRetentionRunner(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			calls.Add(1)
			writeLoanHistoryPage(writer, loanHistoryPageFixture(0, 200))
		}))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		runner.api.SetObserver(func(string, string, int, time.Duration, []byte, []byte, string) {
			cancel()
		})
		raw, err := readLoanHistoryPages(ctx, runner.api)
		if !errors.Is(err, context.Canceled) || raw != nil || calls.Load() != 1 {
			t.Fatalf("canceled history retained its first page or kept requesting: bytes=%d, calls=%d, err=%v", len(raw), calls.Load(), err)
		}
	})

	t.Run("in-flight later page", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := make(chan struct{})
		var calls atomic.Int32
		runner := newRetentionRunner(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if calls.Add(1) == 1 {
				writeLoanHistoryPage(writer, loanHistoryPageFixture(0, 200))
				return
			}
			close(started)
			<-request.Context().Done()
		}))
		type captureResult struct {
			raw json.RawMessage
			err error
		}
		finished := make(chan captureResult, 1)
		go func() {
			raw, err := readLoanHistoryPages(ctx, runner.api)
			finished <- captureResult{raw: raw, err: err}
		}()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("history did not reach its second HTTP page")
		}
		cancel()
		select {
		case result := <-finished:
			if !errors.Is(result.err, context.Canceled) || result.raw != nil || calls.Load() != 2 {
				t.Fatalf("in-flight cancellation retained partial history: bytes=%d, calls=%d, err=%v", len(result.raw), calls.Load(), result.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("history did not honor in-flight HTTP cancellation")
		}
	})
}

func TestReadLoanHistoryPagesDiscardsPartialOnHTTPFailure(t *testing.T) {
	var calls atomic.Int32
	runner := newRetentionRunner(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if calls.Add(1) == 1 {
			writeLoanHistoryPage(writer, loanHistoryPageFixture(0, 200))
			return
		}
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"code":"history_unavailable"}`))
	}))

	raw, err := readLoanHistoryPages(context.Background(), runner.api)
	var apiErr *gameapi.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound || raw != nil || calls.Load() != 2 {
		t.Fatalf("failed later page retained partial history: bytes=%d, calls=%d, err=%v", len(raw), calls.Load(), err)
	}
}

func TestReadLoanHistoryPagesRejectsInvalidLists(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "null", body: `null`},
		{name: "object", body: `{"id":"not-an-array"}`},
		{name: "invalid JSON", body: `[`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			runner := newRetentionRunner(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				_, _ = writer.Write([]byte(test.body))
			}))
			raw, err := readLoanHistoryPages(context.Background(), runner.api)
			if err == nil || raw != nil || calls.Load() != 1 {
				t.Fatalf("invalid history list returned data or retried: bytes=%d, calls=%d, err=%v", len(raw), calls.Load(), err)
			}
		})
	}
}
