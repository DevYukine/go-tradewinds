package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

type fetchKind uint8

const (
	fetchJSON fetchKind = iota
	fetchPages
	fetchContracts
	fetchBorrowerLoans
)

type fetchTask struct {
	key  string
	path string
	kind fetchKind
}

type fetchResult struct {
	data        json.RawMessage
	err         error
	attemptedAt time.Time
	completedAt time.Time
}

// A worker owns its result slot through pagination and the client's existing
// retries and rate waits. Only the caller reduces results into runner state.
func fetchWave(ctx context.Context, api *gameapi.Client, tasks []fetchTask) ([]fetchResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(tasks) == 0 {
		return nil, nil
	}
	results := make([]fetchResult, len(tasks))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(6, len(tasks)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				if ctx.Err() != nil {
					return
				}
				task := tasks[index]
				result := fetchResult{attemptedAt: time.Now()}
				switch task.kind {
				case fetchPages:
					result.data, result.err = readPages(ctx, api, task.path)
				case fetchContracts:
					result.data, result.err = readContracts(ctx, api)
				case fetchBorrowerLoans:
					result.data, result.err = readBorrowerLoans(ctx, api)
				default:
					result.err = api.Get(ctx, task.path, &result.data)
				}
				if result.err == nil && (!json.Valid(result.data) || string(result.data) == "null") {
					result.err = errors.New("upstream response is not the documented non-null JSON dataset")
				}
				if result.err == nil {
					result.err = validatePassengerCapture(task.key, result.data)
				}
				result.completedAt = time.Now()
				results[index] = result
			}
		}()
	}

admit:
	for index := range tasks {
		select {
		case jobs <- index:
		case <-ctx.Done():
			break admit
		}
	}
	close(jobs)
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func readPages(ctx context.Context, api *gameapi.Client, path string) (json.RawMessage, error) {
	endpoint, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Set("limit", "200")
	rows := make([]json.RawMessage, 0)
	seen := make(map[string]bool)
	for {
		endpoint.RawQuery = query.Encode()
		var page []json.RawMessage
		if err := api.Get(ctx, endpoint.String(), &page); err != nil {
			return nil, err
		}
		if page == nil {
			return nil, errors.New("paginated list returned null instead of an array")
		}
		for _, row := range page {
			id, err := pageID(row)
			if err != nil {
				return nil, err
			}
			if seen[id] {
				return nil, fmt.Errorf("pagination repeated id %s", id)
			}
			seen[id] = true
			rows = append(rows, row)
		}
		if len(page) < 200 {
			return json.Marshal(rows)
		}
		id, err := pageID(page[len(page)-1])
		if err != nil {
			return nil, err
		}
		query.Set("cursor", id)
	}
}

func readBorrowerLoans(ctx context.Context, api *gameapi.Client) (json.RawMessage, error) {
	byID := make(map[string]json.RawMessage)
	var order []string
	for _, status := range []string{"active", "defaulted"} {
		rows, err := readPages(ctx, api, "/v1/loan/list?role=borrower&status="+status)
		if err != nil {
			return nil, err
		}
		var page []json.RawMessage
		if err := json.Unmarshal(rows, &page); err != nil {
			return nil, err
		}
		for _, row := range page {
			id, err := pageID(row)
			if err != nil {
				return nil, err
			}
			if _, exists := byID[id]; !exists {
				order = append(order, id)
			}
			// A later default observation supersedes the same active loan.
			byID[id] = row
		}
	}
	combined := make([]json.RawMessage, 0, len(order))
	for _, id := range order {
		combined = append(combined, byID[id])
	}
	return json.Marshal(combined)
}

func readContracts(ctx context.Context, api *gameapi.Client) (json.RawMessage, error) {
	byID := make(map[string]json.RawMessage)
	var order []string
	for _, status := range []string{"open", "active", "completed", "cancelled", "defaulted"} {
		rows, err := readPages(ctx, api, "/v1/contract/list?status="+status)
		if err != nil {
			return nil, err
		}
		var page []json.RawMessage
		if err := json.Unmarshal(rows, &page); err != nil {
			return nil, err
		}
		for _, row := range page {
			id, err := pageID(row)
			if err != nil {
				return nil, err
			}
			if _, exists := byID[id]; !exists {
				order = append(order, id)
			}
			// Later serial status observations supersede earlier visibility.
			byID[id] = row
		}
	}
	combined := make([]json.RawMessage, 0, len(order))
	for _, id := range order {
		combined = append(combined, byID[id])
	}
	return json.Marshal(combined)
}
