package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

// Rival finance is optional intelligence, never authority for a fleet mutation.
// Successful observations go through the committed request observer into the
// permanent store, rather than growing the operational account snapshot.
func (r *Runner) refreshRivalCredit(ctx context.Context) error {
	if time.Now().Before(r.rivalCreditNextLoad) {
		return nil
	}
	r.rivalCreditNextLoad = time.Now().UTC().Add(time.Hour)
	candidates, err := r.db.RivalCreditCandidates(ctx, r.account.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		r.logRivalCreditError(ctx, "candidates", err)
		return ctx.Err()
	}
	ids := make(map[string]bool, len(candidates))
	for _, id := range candidates {
		ids[id] = true
	}
	for _, key := range []string{"monopolies", "futures"} {
		addRivalCreditIDs(ids, r.snapshot[key])
	}
	for _, path := range []string{"/v1/bank/list", "/v1/loan/requests"} {
		var raw json.RawMessage
		if path == "/v1/loan/requests" {
			raw, err = readPages(ctx, r.api, path)
		} else {
			err = r.api.GetOnce(ctx, path, &raw)
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.logRivalCreditError(ctx, path, err)
			continue
		}
		addRivalCreditIDs(ids, raw)
	}
	// Preserve the store's rank/least-recently-observed order; append newly
	// discovered parties without displacing ranked companies.
	ordered := make([]string, 0, len(ids))
	for _, id := range candidates {
		if ids[id] {
			ordered = append(ordered, id)
			delete(ids, id)
		}
	}
	retained := len(ordered)
	for id := range ids {
		if validRivalCreditID(id) && id != r.account.CompanyID {
			ordered = append(ordered, id)
		}
	}
	sort.Strings(ordered[retained:])
	reserved, err := r.db.ReserveRivalCredits(ctx, r.account.ID, ordered)
	if err != nil {
		r.logRivalCreditError(ctx, "reservation", err)
		return ctx.Err()
	}
	for _, id := range reserved {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := "/v1/loan/credit?company_id=" + url.QueryEscape(id)
		var report gameapi.CreditReport
		// GetOnce is essential: transport/5xx retries would consume an unreserved
		// lookup and violate both durable allowances.
		if err := r.api.GetOnce(ctx, path, &report); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.logRivalCreditError(ctx, path, err)
		}
	}
	return nil
}

func addRivalCreditIDs(ids map[string]bool, raw json.RawMessage) {
	var rows []struct {
		CompanyID  string `json:"company_id"`
		HolderID   string `json:"holder_company_id"`
		BorrowerID string `json:"borrower_company_id"`
		CreatorID  string `json:"creator_company_id"`
		SellerID   string `json:"seller_company_id"`
		BuyerID    string `json:"buyer_company_id"`
	}
	if json.Unmarshal(raw, &rows) != nil {
		return
	}
	for _, row := range rows {
		for _, id := range []string{row.CompanyID, row.HolderID, row.BorrowerID, row.CreatorID, row.SellerID, row.BuyerID} {
			if validRivalCreditID(id) {
				ids[id] = true
			}
		}
	}
}

func validRivalCreditID(id string) bool {
	return strings.TrimSpace(id) != "" && len(id) <= 255
}

func (r *Runner) logRivalCreditError(ctx context.Context, path string, err error) {
	if entry := r.log.Check(zap.WarnLevel, "rival credit observation failed"); entry != nil {
		entry.Write(r.scrubOutcomeFields(ctx, nil, []outcomeText{
			{field: "path", value: path}, {field: "cause", value: err.Error()},
		})...)
	}
}
