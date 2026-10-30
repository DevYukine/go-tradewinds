package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

// UnderworldAPI deliberately grants no mutation capability. The runtime supplies
// the separately scoped observer key and a single-attempt GET implementation.
type UnderworldAPI interface {
	Get(context.Context, string, any) error
}

func (r *Runner) disableUnderworld(reason string) {
	if r.underworldDisabled {
		return
	}
	r.underworldDisabled = true
	r.log.Warn("underworld observer disabled", zap.String("reason", reason))
}

func (r *Runner) refreshUnderworld(ctx context.Context, state AccountState) error {
	if err := r.refreshUnderworldVisits(ctx, state); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.log.Warn("underworld observation failed", r.scrubOutcomeFields(ctx, nil,
			[]outcomeText{{field: "cause", value: err.Error()}})...)
		r.disableUnderworld("observer storage or payload unavailable; trading continues")
	}
	return nil
}

func (r *Runner) refreshUnderworldVisits(ctx context.Context, state AccountState) error {
	if r.underworldDisabled {
		return nil
	}
	if r.opts.UnderworldAPI == nil {
		r.disableUnderworld("missing observer key; run underworld-key")
		return nil
	}
	if !r.fresh("ships") || !r.fresh("catalog") {
		return nil
	}
	goods := make([]string, 0, store.UnderworldBooksPerVisit)
	for _, commodity := range state.Catalog.Commodities {
		if commodity.Category == "contraband" && commodity.ID != "" {
			goods = append(goods, commodity.ID)
		}
	}
	slices.Sort(goods)
	goods = slices.Compact(goods)
	if len(goods) > store.UnderworldBooksPerVisit {
		goods = goods[:store.UnderworldBooksPerVisit]
	}
	for _, ship := range state.Ships {
		if err := ctx.Err(); err != nil {
			return err
		}
		if ship.Ship.ID == "" {
			continue
		}
		portID := ""
		if ship.Voyage == nil {
			portID = ship.Ship.CurrentPortID
		}
		visitID, err := r.db.TrackUnderworldVisit(ctx, r.account.ID, ship.Ship.ID, portID)
		if err != nil {
			return fmt.Errorf("track underworld visit: %w", err)
		}
		port, exists := findPort(state.Catalog, portID)
		if portID == "" || !exists || (ship.Ship.Status != "berthed" && ship.Ship.Status != "anchored_roadstead") {
			continue
		}
		if err := r.observeUnderworld(ctx, ship.Ship.ID, visitID, portID, "wants", ""); err != nil {
			return err
		}
		if r.underworldDisabled {
			return nil
		}
		if !port.IsCove {
			continue
		}
		for _, commodityID := range goods {
			if err := r.observeUnderworld(ctx, ship.Ship.ID, visitID, portID, "book", commodityID); err != nil {
				return err
			}
			if r.underworldDisabled {
				return nil
			}
		}
	}
	return nil
}

func (r *Runner) observeUnderworld(ctx context.Context, shipID, visitID, portID, kind, commodityID string) error {
	reserved, err := r.db.ReserveUnderworldRead(ctx, r.account.ID, shipID, visitID, kind, commodityID)
	if err != nil {
		return fmt.Errorf("reserve underworld read: %w", err)
	}
	if !reserved {
		return nil
	}
	query := url.Values{"port_id": {portID}}
	if commodityID != "" {
		query.Set("commodity_id", commodityID)
	}
	var payload json.RawMessage
	if err := r.opts.UnderworldAPI.Get(ctx, "/v1/underworld/"+kind+"?"+query.Encode(), &payload); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var apiErr *gameapi.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized &&
			(apiErr.Code == "revoked_api_key" || apiErr.Code == "expired_api_key" || apiErr.Code == "invalid_api_key") {
			r.disableUnderworld("observer key rejected; run underworld-key")
			return nil
		}
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden && apiErr.Code == "missing_scope" {
			r.disableUnderworld("observer key is missing the required scope")
			return nil
		}
		r.log.Warn("underworld observation failed", r.scrubOutcomeFields(ctx,
			[]zap.Field{zap.String("kind", kind)},
			[]outcomeText{{field: "ship_id", value: shipID}, {field: "port_id", value: portID}, {field: "cause", value: err.Error()}})...)
		return nil
	}
	if err := r.db.RecordUnderworldObservation(ctx, &store.UnderworldObservation{
		AccountID: r.account.ID, ShipID: shipID, VisitID: visitID, PortID: portID,
		Kind: kind, CommodityID: commodityID, ObservedAt: time.Now().UTC(), Payload: payload,
	}); err != nil {
		return fmt.Errorf("persist underworld observation: %w", err)
	}
	return nil
}
