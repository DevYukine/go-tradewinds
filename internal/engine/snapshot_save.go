package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

func (r *Runner) saveSnapshot(ctx context.Context) error {
	err := r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot)
	if err == nil {
		r.snapshotShed = ""
		return nil
	}
	if !errors.Is(err, store.ErrSnapshotRowTooLarge) {
		return err
	}

	// Only the persisted copy loses observations; live planning keeps fresh data.
	snapshot := make(map[string]json.RawMessage, len(r.snapshot))
	for key, raw := range r.snapshot {
		snapshot[key] = raw
	}

	var metadata map[string]DatasetMeta
	if raw := snapshot["_meta"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &metadata); err != nil {
			return fmt.Errorf("decode snapshot metadata for size fallback: %w", err)
		}
	}
	if metadata == nil {
		metadata = make(map[string]DatasetMeta)
	}
	var dropped []string
	var droppedBytes []int
	// Ports go last because the persisted copy also supplies dashboard readers.
	for _, dataset := range []string{
		"world_events", "books", "insurance", "insurance_owned", "fills", "ports",
	} {
		raw, exists := snapshot[dataset]
		if !exists {
			continue
		}
		delete(snapshot, dataset)
		dropped = append(dropped, dataset)
		droppedBytes = append(droppedBytes, len(dataset)+len(raw))
		meta := metadata[dataset]
		meta.Stale = true
		meta.Complete = false
		meta.Error = store.SnapshotShedError
		metadata[dataset] = meta
		prefix := dataset + "/"
		for key, child := range metadata {
			if strings.HasPrefix(key, prefix) {
				child.Stale = true
				child.Complete = false
				child.Error = store.SnapshotShedError
				metadata[key] = child
			}
		}
		encoded, encodeErr := json.Marshal(metadata)
		if encodeErr != nil {
			return fmt.Errorf("encode snapshot metadata for size fallback: %w", encodeErr)
		}
		snapshot["_meta"] = encoded
		err = r.db.SaveSnapshot(ctx, r.account.ID, snapshot)
		if !errors.Is(err, store.ErrSnapshotRowTooLarge) {
			break
		}
	}
	if len(dropped) != 0 {
		datasets := strings.Join(dropped, ", ")
		level := zap.ErrorLevel
		if datasets == r.snapshotShed {
			level = zap.DebugLevel
		}
		r.snapshotShed = datasets
		if entry := r.log.Check(level, "snapshot over size cap; saving without refetchable datasets"); entry != nil {
			entry.Write(
				zap.Int("limit_bytes", store.MaxSnapshotBytes),
				zap.Ints("dropped_bytes", droppedBytes),
				zap.String("dropped_datasets", datasets),
			)
		}
	}
	if errors.Is(err, store.ErrSnapshotRowTooLarge) {
		type rowKeySize struct {
			key   string
			bytes int
		}
		largest := make([]rowKeySize, 0, len(snapshot))
		for key, raw := range snapshot {
			if key != "routes" {
				largest = append(largest, rowKeySize{key: key, bytes: len(key) + len(raw)})
			}
		}
		sort.Slice(largest, func(i, j int) bool {
			if largest[i].bytes != largest[j].bytes {
				return largest[i].bytes > largest[j].bytes
			}
			return largest[i].key < largest[j].key
		})
		largest = largest[:min(5, len(largest))]
		keys := make([]string, len(largest))
		for i, item := range largest {
			keys[i] = item.key
		}
		keys = r.db.ScrubOutcomeTexts(ctx, r.account.ID, keys)
		details := make([]string, len(largest))
		for i, item := range largest {
			details[i] = fmt.Sprintf("%s=%d bytes", keys[i], item.bytes)
		}
		return fmt.Errorf("save snapshot after shedding; largest row keys: %s: %w", strings.Join(details, ", "), err)
	}
	return err
}
