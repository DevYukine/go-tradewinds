package httpapi

import (
	"context"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

type BuildIdentity struct {
	Revision  *string   `json:"revision"`
	Modified  bool      `json:"modified"`
	StartedAt time.Time `json:"started_at"`
}

var processBuild = readBuildIdentity()

func readBuildIdentity() BuildIdentity {
	identity := BuildIdentity{StartedAt: time.Now().UTC()}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if setting.Value != "" {
					revision := setting.Value
					identity.Revision = &revision
				}
			case "vcs.modified":
				identity.Modified = setting.Value == "true"
			}
		}
	}
	return identity
}

// CurrentBuildIdentity is shared by the API and its startup log. A build with
// no VCS metadata remains explicitly unidentified.
func CurrentBuildIdentity() BuildIdentity {
	return processBuild
}

func (h *handler) warnings(ctx context.Context, w http.ResponseWriter) {
	if h.store == nil {
		h.failure(w, http.StatusServiceUnavailable, "database_unavailable", "PostgreSQL is not ready.")
		return
	}
	at := time.Now().UTC()
	report, err := h.store.WarningReport(ctx, at)
	if err != nil {
		if ctx.Err() != context.Canceled {
			h.readLog.Warn("read warnings failed", zap.Error(err))
			h.failure(w, http.StatusServiceUnavailable, "database_unavailable", "The database read failed; no empty success was substituted.")
		}
		return
	}
	h.json(w, http.StatusOK, struct {
		GeneratedAt time.Time     `json:"generated_at"`
		Build       BuildIdentity `json:"build"`
		store.WarningReport
	}{GeneratedAt: at, Build: CurrentBuildIdentity(), WarningReport: report})
}
