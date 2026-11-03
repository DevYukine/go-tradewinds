package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func (h *handler) loans(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	query, err := parseLoanQuery(r)
	if err != nil {
		h.failure(w, http.StatusBadRequest, "invalid_account", "account_id must be a single nonblank account identifier of at most 255 bytes when supplied.")
		return
	}
	if h.store == nil {
		h.failure(w, http.StatusServiceUnavailable, "database_unavailable", "PostgreSQL is not ready.")
		return
	}
	data, err := h.store.Loans(ctx, query)
	if err != nil {
		if errors.Is(err, store.ErrResponseTooLarge) {
			h.failure(w, http.StatusServiceUnavailable, "response_too_large", "Loan history too large to display.")
			return
		}
		h.readError(ctx, w, "loans", err)
		return
	}
	h.json(w, http.StatusOK, data)
}

func parseLoanQuery(r *http.Request) (string, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", err
	}
	values, supplied := query["account_id"]
	if !supplied {
		return "", nil
	}
	if len(values) != 1 || len(values[0]) > 255 || strings.TrimSpace(values[0]) == "" || strings.IndexByte(values[0], 0) >= 0 {
		return "", store.ErrInvalidInput
	}
	return values[0], nil
}
