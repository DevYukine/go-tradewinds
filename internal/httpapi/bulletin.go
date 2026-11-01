package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func (h *handler) bulletinBoard(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	query := store.BulletinQuery{Search: values.Get("q"), Company: values.Get("company")}
	if raw := values.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 500 {
			h.failure(w, http.StatusBadRequest, "invalid_query", "limit must be an integer from 1 through 500.")
			return
		}
		query.Limit = limit
	}
	if utf8.RuneCountInString(query.Search) > 200 {
		h.failure(w, http.StatusBadRequest, "invalid_query", "q must contain at most 200 characters.")
		return
	}
	if h.store == nil {
		h.failure(w, http.StatusServiceUnavailable, "database_unavailable", "PostgreSQL is not ready.")
		return
	}
	board, err := h.store.BulletinBoard(ctx, query)
	if err != nil {
		h.readError(ctx, w, "bulletin", err)
		return
	}
	h.json(w, http.StatusOK, board)
}

func (h *handler) bulletinThread(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if uuid.Validate(id) != nil {
		h.failure(w, http.StatusBadRequest, "invalid_query", "id must be a thread UUID.")
		return
	}
	if h.store == nil {
		h.failure(w, http.StatusServiceUnavailable, "database_unavailable", "PostgreSQL is not ready.")
		return
	}
	thread, err := h.store.BulletinThread(ctx, id)
	if err != nil {
		h.readError(ctx, w, "bulletin thread", err)
		return
	}
	h.json(w, http.StatusOK, thread)
}
