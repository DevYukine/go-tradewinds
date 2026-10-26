package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (h *handler) standingsHistory(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	accountID := query.Get("account_id")
	if len(accountID) > 255 || strings.TrimSpace(accountID) == "" {
		h.failure(w, http.StatusBadRequest, "invalid_account", "standings history requires an account_id.")
		return
	}
	to := time.Now().UTC()
	var from time.Time
	switch query.Get("range") {
	case "24h":
		from = to.Add(-24 * time.Hour)
	case "7d":
		from = to.Add(-7 * 24 * time.Hour)
	case "30d":
		from = to.Add(-30 * 24 * time.Hour)
	case "all":
	default:
		h.failure(w, http.StatusBadRequest, "invalid_range", "range must be 24h, 7d, 30d, or all.")
		return
	}
	if query.Has("from") || query.Has("to") {
		h.failure(w, http.StatusBadRequest, "invalid_range", "standings history uses range presets, not from or to overrides.")
		return
	}
	top := 5
	if raw := query.Get("top"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 20 {
			h.failure(w, http.StatusBadRequest, "invalid_top", "top must be an integer from 1 through 20.")
			return
		}
		top = parsed
	}
	if h.store == nil {
		h.failure(w, http.StatusServiceUnavailable, "database_unavailable", "PostgreSQL is not ready.")
		return
	}
	data, err := h.store.StandingsHistory(ctx, accountID, from, to, top)
	if err != nil {
		h.readError(w, "standings history", err)
		return
	}
	h.json(w, http.StatusOK, data)
}
