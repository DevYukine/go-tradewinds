package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

const (
	loanHistoryPageSize = 200
	loanHistoryMaxPages = 20
	loanHistoryMaxRows  = 4000
	loanHistoryMaxBytes = store.MaxSnapshotBytes
)

// History is an optional report capture, not operational debt authority. Keep
// malformed rows for its consumer to report without allowing an unbounded scan.
func readLoanHistoryPages(ctx context.Context, api *gameapi.Client) (json.RawMessage, error) {
	query := url.Values{
		"role":  {"borrower"},
		"limit": {strconv.Itoa(loanHistoryPageSize)},
	}
	rows := make([]json.RawMessage, 0)
	seen := make(map[string]bool)
	encodedBytes := 2 // Opening and closing array brackets.
	for range loanHistoryMaxPages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var page json.RawMessage
		if err := api.Get(ctx, "/v1/loan/list?"+query.Encode(), &page); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Do not let an oversized array allocate one slice slot per upstream
		// element before its row budget can be checked.
		decoder := json.NewDecoder(bytes.NewReader(page))
		opening, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		if opening != json.Delim('[') {
			return nil, errors.New("loan history did not return an array")
		}
		pageRows := 0
		cursor := ""
		for decoder.More() {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if len(rows) >= loanHistoryMaxRows {
				return nil, fmt.Errorf("loan history exceeds its %d-row capture budget: %w", loanHistoryMaxRows, store.ErrResponseTooLarge)
			}
			if pageRows >= loanHistoryPageSize {
				return nil, fmt.Errorf("loan history page exceeds its %d-row limit: %w", loanHistoryPageSize, store.ErrResponseTooLarge)
			}
			var row json.RawMessage
			if err := decoder.Decode(&row); err != nil {
				return nil, err
			}
			pageRows++
			rowBytes := len(row)
			if len(rows) > 0 {
				rowBytes++ // Separator from the preceding row, including across pages.
			}
			if rowBytes > loanHistoryMaxBytes-encodedBytes {
				return nil, fmt.Errorf("loan history exceeds its %d-byte capture budget: %w", loanHistoryMaxBytes, store.ErrResponseTooLarge)
			}
			// RawMessage marshaling escapes HTML and Unicode line separators.
			// Charge that expansion before retaining the row or its ID. Counting
			// the original whitespace also bounds malformed rows' input bytes.
			for index, value := range row {
				extra := 0
				switch value {
				case '<', '>', '&':
					extra = 5
				case 0xe2:
					if index+2 < len(row) && row[index+1] == 0x80 && (row[index+2] == 0xa8 || row[index+2] == 0xa9) {
						extra = 3
					}
				}
				if extra > loanHistoryMaxBytes-encodedBytes-rowBytes {
					return nil, fmt.Errorf("loan history exceeds its %d-byte capture budget: %w", loanHistoryMaxBytes, store.ErrResponseTooLarge)
				}
				rowBytes += extra
			}
			encodedBytes += rowBytes
			if id, err := pageID(row); err == nil {
				if seen[id] {
					return nil, errors.New("loan history pagination repeated an id or cursor")
				}
				seen[id] = true
				cursor = id
			}
			rows = append(rows, row)
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		if pageRows < loanHistoryPageSize {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return json.Marshal(rows)
		}
		if cursor == "" {
			return nil, errors.New("loan history full page has no usable cursor")
		}
		query.Set("cursor", cursor)
	}
	return nil, fmt.Errorf("loan history exceeds its %d-page capture budget: %w", loanHistoryMaxPages, store.ErrResponseTooLarge)
}
