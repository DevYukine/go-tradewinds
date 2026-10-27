package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	MaxReplayCompressedBytes = 4 << 20
	MaxReplayPlaintextBytes  = 16 << 20
	MaxReplayEventCount      = 256
	MaxReplayEventBytes      = 1 << 20
	replayLastSeenSlack      = time.Hour
	replayEncodingGzip       = "gzip"
	replayEncodingEvents     = "events-json"
	replayFrameBudgetCause   = "replay frame exceeds storage byte budget"
	replayRedactionSizeCause = "replay dataset exceeds storage byte budget after redaction"
	replayCompressedCause    = "replay payload exceeds 4 MiB compressed"
)

type ReplayDataset struct {
	Data []byte
	Meta json.RawMessage
}

type ReplayFrame struct {
	ID             int64
	AccountID      string
	Generation     string
	Source         string
	ObservedAt     time.Time
	Clock          json.RawMessage
	Phase          int
	CatalogDigest  string
	Decision       bool
	AssumedCatalog bool
	Datasets       map[string]ReplayDataset
}

type ReplayFrameWrite struct {
	ID              int64
	PayloadsAdded   int64
	DemotedDatasets int64
}

// boundReplayFrame replaces the largest payloads first with disclosed gaps.
// Frames already inside both the raw and manifest budgets retain their slices
// and map. If names alone cannot fit, one compact frame gap replaces the map.
func boundReplayFrame(frame ReplayFrame) (ReplayFrame, int64) {
	rawBytes := int64(len(frame.Clock)) + int64(len(frame.Generation)) + int64(len(frame.CatalogDigest))
	manifestBytes := int64(2)
	for key, dataset := range frame.Datasets {
		rawBytes += int64(len(key)) + int64(len(dataset.Data)) + int64(len(dataset.Meta))
		// Metadata is base64 encoded in the manifest; keys may need JSON escaping.
		manifestBytes += int64(len(key))*6 + int64((len(dataset.Meta)+2)/3)*4 + 256
	}
	originalBytes := rawBytes
	if rawBytes <= MaxReplayPlaintextBytes && manifestBytes <= MaxReplayCompressedBytes-1024 {
		return frame, 0
	}
	// The raw clock is duplicated in the encrypted clock dataset. Do not parse a
	// giant public-header copy merely to discover it must be discarded.
	if len(frame.Clock) > 64<<10 {
		rawBytes -= int64(len(frame.Clock)) - 4
		frame.Clock = json.RawMessage("null")
	}
	if rawBytes <= MaxReplayPlaintextBytes && manifestBytes <= MaxReplayCompressedBytes-1024 {
		return frame, 0
	}
	keys := make([]string, 0, len(frame.Datasets))
	for key := range frame.Datasets {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := len(frame.Datasets[keys[i]].Data), len(frame.Datasets[keys[j]].Data)
		if left != right {
			return left > right
		}
		return keys[i] < keys[j]
	})
	datasets := make(map[string]ReplayDataset, len(frame.Datasets))
	for key, dataset := range frame.Datasets {
		datasets[key] = dataset
	}
	frame.Datasets = datasets
	var gaps int64
	for _, key := range keys {
		if rawBytes <= MaxReplayPlaintextBytes && manifestBytes <= MaxReplayCompressedBytes-1024 {
			break
		}
		dataset := datasets[key]
		if len(dataset.Data) == 0 && len(dataset.Meta) <= 64<<10 {
			continue
		}
		gap := replayDatasetGap(dataset, replayFrameBudgetCause, false)
		rawBytes += int64(len(gap.Meta)) - int64(len(dataset.Meta)) - int64(len(dataset.Data))
		manifestBytes += int64((len(gap.Meta)+2)/3)*4 - int64((len(dataset.Meta)+2)/3)*4
		datasets[key] = gap
		gaps++
	}
	if rawBytes > MaxReplayPlaintextBytes || manifestBytes > MaxReplayCompressedBytes-1024 {
		// Keep no unbounded keys or digest strings in this last-resort summary.
		// Authenticated storage/redaction still applies to the compact gap.
		meta, _ := json.Marshal(struct {
			Complete         bool   `json:"complete"`
			Stale            bool   `json:"stale"`
			Error            string `json:"error"`
			OriginalBytes    int64  `json:"original_bytes"`
			OriginalDatasets int    `json:"original_datasets"`
		}{false, true, "replay frame names or metadata exceed storage byte budget", originalBytes, len(datasets)})
		frame.Datasets = map[string]ReplayDataset{"_replay_gap": {Meta: meta}}
		frame.CatalogDigest = ""
		frame.Clock = json.RawMessage("null")
		gaps = int64(len(datasets))
	}
	return frame, gaps
}

func replayDatasetGap(dataset ReplayDataset, cause string, discardMeta bool) ReplayDataset {
	// Never parse giant metadata, or retain metadata whose redaction failed.
	var meta map[string]json.RawMessage
	if !discardMeta && len(dataset.Meta) <= 64<<10 {
		_ = json.Unmarshal(dataset.Meta, &meta)
	}
	if meta == nil {
		meta = make(map[string]json.RawMessage)
	}
	meta["complete"] = json.RawMessage("false")
	meta["stale"] = json.RawMessage("true")
	meta["error"], _ = json.Marshal(cause)
	meta["original_bytes"] = json.RawMessage(strconv.Itoa(len(dataset.Data)))
	if discardMeta || len(dataset.Meta) > 64<<10 {
		meta["original_meta_bytes"] = json.RawMessage(strconv.Itoa(len(dataset.Meta)))
	}
	// Adding gap fields must not expand otherwise-safe metadata through HTML
	// escaping after the sanitized frame has already been bounded.
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(meta)
	return ReplayDataset{Meta: bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))}
}

type ReplayPlan struct {
	ID         int64
	AccountID  string
	MissionID  string
	ShipID     string
	Phase      string
	Generation string
	Sequence   int64
	ObservedAt time.Time
	Plan       json.RawMessage
	Policy     json.RawMessage
	Revision   string
}

type ReplayActionLink struct {
	AccountID  string `gorm:"primaryKey"`
	ActionID   string `gorm:"primaryKey"`
	MissionID  string
	Stage      string
	Sequence   int64
	Generation string
}

func (ReplayActionLink) TableName() string { return "replay_action_links" }

type replayManifestEntry struct {
	Digest   string `json:"digest,omitempty"`
	Encoding string `json:"encoding,omitempty"`
	Meta     []byte `json:"meta"`
}

type replayFrameRow struct {
	ID             int64
	CaptureID      string
	AccountID      string
	Generation     string
	Source         string
	ObservedAt     time.Time
	Clock          json.RawMessage `gorm:"type:jsonb"`
	Phase          int
	CatalogDigest  string
	Decision       bool
	AssumedCatalog bool
	ManifestCipher []byte
	StoredBytes    int64
}

func (replayFrameRow) TableName() string { return "replay_frames" }

type replayPayloadRow struct {
	AccountID     string `gorm:"primaryKey"`
	Digest        string `gorm:"primaryKey"`
	PayloadCipher []byte
	StoredBytes   int64
	LastSeenAt    time.Time
}

func (replayPayloadRow) TableName() string { return "replay_payloads" }

type replayPayloadInput struct {
	Data     []byte
	Encoding string
}

// Only documented numeric, timestamp and boolean clock scalars belong in the
// public header. Extensions and string labels remain in the encrypted dataset.
type replayPublicClock struct {
	RealUTC        *time.Time `json:"real_utc,omitempty"`
	UnlockedPhase  *int64     `json:"unlocked_phase,omitempty"`
	Paused         *bool      `json:"paused,omitempty"`
	TotalGameHours *int64     `json:"total_game_hours,omitempty"`
	Phase          *int64     `json:"phase,omitempty"`
	Calendar       *struct {
		Year           *int64 `json:"year,omitempty"`
		Month          *int64 `json:"month,omitempty"`
		Day            *int64 `json:"day,omitempty"`
		Hour           *int64 `json:"hour,omitempty"`
		Minute         *int64 `json:"minute,omitempty"`
		TimeScale      *int64 `json:"time_scale,omitempty"`
		TotalGameHours *int64 `json:"total_game_hours,omitempty"`
	} `json:"calendar,omitempty"`
}

func replayClockProjection(raw []byte) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage("null"), nil
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, fmt.Errorf("replay public clock: %w", ErrInvalidInput)
	}
	if input == nil {
		return json.RawMessage("null"), nil
	}
	clock := replayPublicClock{}
	// Decode each documented scalar independently: an extension using the
	// wrong type must not hide valid siblings or poison the entire writer.
	if err := json.Unmarshal(input["real_utc"], &clock.RealUTC); err != nil {
		clock.RealUTC = nil
	}
	if err := json.Unmarshal(input["unlocked_phase"], &clock.UnlockedPhase); err != nil {
		clock.UnlockedPhase = nil
	}
	if err := json.Unmarshal(input["paused"], &clock.Paused); err != nil {
		clock.Paused = nil
	}
	if err := json.Unmarshal(input["total_game_hours"], &clock.TotalGameHours); err != nil {
		clock.TotalGameHours = nil
	}
	if err := json.Unmarshal(input["phase"], &clock.Phase); err != nil {
		clock.Phase = nil
	}
	var calendar map[string]json.RawMessage
	if json.Unmarshal(input["calendar"], &calendar) == nil && calendar != nil {
		// Filter wrong-typed calendar members before decoding the typed public
		// shape, retaining exact int64 values rather than float64 intermediates.
		valid := make(map[string]int64)
		for _, key := range []string{"year", "month", "day", "hour", "minute", "time_scale", "total_game_hours"} {
			var value *int64
			if json.Unmarshal(calendar[key], &value) == nil && value != nil {
				valid[key] = *value
			}
		}
		projected, _ := json.Marshal(valid)
		_ = json.Unmarshal(projected, &clock.Calendar)
	}
	if clock.RealUTC != nil {
		utc := clock.RealUTC.UTC()
		clock.RealUTC = &utc
	}
	return json.Marshal(clock)
}

func replayTimestamp(at time.Time) string {
	return at.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
}

func replayManifestAAD(row replayFrameRow) (string, error) {
	// JSONB reorders keys and whitespace. The writer projects integer-only
	// numeric fields, so re-encoding with UseNumber preserves their precision.
	decoder := json.NewDecoder(bytes.NewReader(row.Clock))
	decoder.UseNumber()
	var clock any
	if err := decoder.Decode(&clock); err != nil {
		return "", fmt.Errorf("replay clock header: %w", ErrInvalidInput)
	}
	canonical, err := json.Marshal(clock)
	if err != nil {
		return "", err
	}
	clockDigest := sha256.Sum256(canonical)
	identity := privateFrame("v2", row.CaptureID, row.Generation, row.Source, replayTimestamp(row.ObservedAt),
		hex.EncodeToString(clockDigest[:]), strconv.Itoa(row.Phase), row.CatalogDigest,
		strconv.FormatBool(row.Decision), strconv.FormatBool(row.AssumedCatalog))
	return privateAAD("replay_frames", "manifest", row.AccountID, identity), nil
}

// Inspect every token, including duplicate object members, before deciding that
// exact original bytes are safe to retain. The shared redactor owns scrubbing.
func replayJSONNeedsRedaction(decoder *json.Decoder, secrets []string) (bool, error) {
	token, err := decoder.Token()
	if err != nil {
		return false, err
	}
	switch value := token.(type) {
	case json.Delim:
		for decoder.More() {
			if value == '{' {
				key, err := decoder.Token()
				if err != nil {
					return false, err
				}
				name := key.(string)
				if sensitiveKey(name) || scrubText(name, secrets) != name {
					return true, nil
				}
			}
			needed, err := replayJSONNeedsRedaction(decoder, secrets)
			if needed || err != nil {
				return needed, err
			}
		}
		_, err := decoder.Token()
		return false, err
	case string:
		return scrubText(value, secrets) != value, nil
	default:
		return false, nil
	}
}

func replayRedactJSON(raw []byte, secrets []string) ([]byte, error) {
	if len(raw) > MaxReplayPlaintextBytes {
		return nil, ErrResponseTooLarge
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	needed, err := replayJSONNeedsRedaction(decoder, secrets)
	if err == nil && !needed {
		if _, err = decoder.Token(); err == io.EOF {
			return raw, nil
		}
	}
	if !needed {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '{' && trimmed[0] != '[' {
			text := string(raw)
			if scrubText(text, secrets) == text {
				return raw, nil
			}
		}
	}
	clean, err := redactJSON(raw, secrets, 0)
	if err != nil {
		return nil, err
	}
	if len(clean) > MaxReplayPlaintextBytes {
		return nil, ErrResponseTooLarge
	}
	return clean, nil
}

// Stream the length-framed identity, including the payload, without copying it
// into a payload-sized string. This uses the private-evidence lookup key.
func (s *Store) replayDigest(accountID string, data []byte) string {
	return s.replayEncodedDigest(accountID, replayEncodingGzip, data)
}

func (s *Store) replayEncodedDigest(accountID, encoding string, data []byte) string {
	mac := hmac.New(sha256.New, s.lookupKey)
	_, _ = mac.Write([]byte(privateFrame("tradewinds", "private-evidence", "v2", "replay-payload", accountID, encoding)))
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(data)))
	_, _ = mac.Write(size[:])
	_, _ = mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

func replayCompress(data []byte) ([]byte, error) {
	if len(data) > MaxReplayPlaintextBytes {
		return nil, ErrResponseTooLarge
	}
	var buffer bytes.Buffer
	writer, err := gzip.NewWriterLevel(&buffer, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(data); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func replayDecompress(data []byte) ([]byte, error) {
	if len(data) > MaxReplayCompressedBytes {
		return nil, ErrResponseTooLarge
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	plain, err := io.ReadAll(io.LimitReader(reader, MaxReplayPlaintextBytes+1))
	if err != nil {
		return nil, err
	}
	if len(plain) > MaxReplayPlaintextBytes {
		return nil, ErrResponseTooLarge
	}
	return plain, nil
}

func (s *Store) encryptReplayManifest(manifest map[string]replayManifestEntry, row replayFrameRow) ([]byte, error) {
	plain, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	compressed, err := replayCompress(plain)
	if err != nil {
		return nil, err
	}
	if len(compressed) > MaxReplayCompressedBytes {
		return nil, ErrResponseTooLarge
	}
	aad, err := replayManifestAAD(row)
	if err != nil {
		return nil, err
	}
	return s.encrypt(compressed, aad)
}

// Recorder errors include queue drops and must be visible even while its sole
// database writer is stalled. These process-lifetime counters require no I/O.
func (s *Store) CountReplayRecordError(accountID string) {
	s.addReplayRecordErrors(accountID, 1)
}

func (s *Store) addReplayRecordErrors(accountID string, count int64) {
	value, exists := s.replayErrors.Load(accountID)
	if !exists {
		value, _ = s.replayErrors.LoadOrStore(accountID, &atomic.Int64{})
	}
	value.(*atomic.Int64).Add(count)
}

func (s *Store) observeReplayDemotions(accountID, cause string, count int64) {
	if count == 0 {
		return
	}
	s.addReplayRecordErrors(accountID, count)
	key := struct{ accountID, cause string }{accountID, cause}
	if _, warned := s.replayWarnings.LoadOrStore(key, struct{}{}); !warned {
		s.log.Warn("replay recording stored dataset gaps", zap.String("account_id", accountID),
			zap.String("cause", cause), zap.Int64("demoted_datasets", count))
	}
}

// Replay writes deliberately use query, not writeOwned: recording must never
// probe or cancel an account's authoritative executor lease.
func (s *Store) RecordReplayFrame(ctx context.Context, frame ReplayFrame) (int64, error) {
	result, err := s.recordReplayFrame(ctx, frame)
	return result.ID, err
}

type replayPreparedFrame struct {
	frame              ReplayFrame
	originalKeys       map[string]string
	demotedDatasets    int64
	redactionDemotions int64
}

func (s *Store) prepareReplayFrame(ctx context.Context, frame ReplayFrame) (replayPreparedFrame, error) {
	if !validID(frame.AccountID) || !validID(frame.Generation) || frame.ObservedAt.IsZero() ||
		(frame.Source != "live" && frame.Source != "diagnostic_backfill") {
		return replayPreparedFrame{}, fmt.Errorf("replay frame fields: %w", ErrInvalidInput)
	}
	if len(frame.Clock) != 0 && len(frame.Clock) <= 64<<10 {
		clock := bytes.TrimSpace(frame.Clock)
		if !json.Valid(clock) || clock[0] != '{' && !bytes.Equal(clock, []byte("null")) {
			return replayPreparedFrame{}, fmt.Errorf("replay public clock: %w", ErrInvalidInput)
		}
	}
	for key, dataset := range frame.Datasets {
		if key == "" || len(dataset.Meta) <= 64<<10 && !json.Valid(dataset.Meta) {
			return replayPreparedFrame{}, fmt.Errorf("replay dataset metadata: %w", ErrInvalidInput)
		}
	}
	originals := frame.Datasets
	frame, rawDemotions := boundReplayFrame(frame)
	keys := make([]string, 0, len(frame.Datasets))
	for key, dataset := range frame.Datasets {
		if !json.Valid(dataset.Meta) {
			return replayPreparedFrame{}, fmt.Errorf("replay dataset metadata: %w", ErrInvalidInput)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	secrets, err := s.secrets(ctx, frame.AccountID)
	if err != nil {
		return replayPreparedFrame{}, fmt.Errorf("read replay credentials: %w", databaseError{cause: err})
	}
	clock, err := replayClockProjection(frame.Clock)
	if err != nil {
		return replayPreparedFrame{}, err
	}
	prepared := replayPreparedFrame{
		frame: frame, demotedDatasets: rawDemotions,
	}
	prepared.frame.Clock = clock
	prepared.frame.Generation = scrubText(frame.Generation, secrets)
	prepared.frame.CatalogDigest = scrubText(frame.CatalogDigest, secrets)
	prepared.frame.Datasets = make(map[string]ReplayDataset, len(frame.Datasets))
	var nextSuffix map[string]int
	var demoted map[string]struct{}
	for _, key := range keys {
		dataset := frame.Datasets[key]
		alreadyDemoted := rawDemotions > 0 && (rawDemotions == int64(len(originals)) ||
			len(dataset.Data) != len(originals[key].Data) || !bytes.Equal(dataset.Meta, originals[key].Meta))
		cleanKey := scrubText(key, secrets)
		if cleanKey != key {
			if prepared.originalKeys == nil {
				prepared.originalKeys = make(map[string]string)
				nextSuffix = make(map[string]int)
			}
			cleanKey = uniqueRedactedKey(cleanKey, frame.Datasets, prepared.originalKeys, nextSuffix)
			prepared.originalKeys[cleanKey] = key
		}
		datasetDemoted := false
		meta, err := replayRedactJSON(dataset.Meta, secrets)
		if errors.Is(err, ErrResponseTooLarge) {
			dataset = replayDatasetGap(dataset, replayRedactionSizeCause, true)
			datasetDemoted = true
		} else if err != nil {
			return replayPreparedFrame{}, err
		} else {
			dataset.Meta = meta
			if len(dataset.Data) != 0 {
				data, err := replayRedactJSON(dataset.Data, secrets)
				if errors.Is(err, ErrResponseTooLarge) {
					dataset = replayDatasetGap(dataset, replayRedactionSizeCause, false)
					datasetDemoted = true
				} else if err != nil {
					return replayPreparedFrame{}, err
				} else {
					dataset.Data = data
				}
			}
		}
		if alreadyDemoted || datasetDemoted {
			if demoted == nil {
				demoted = make(map[string]struct{})
			}
			demoted[cleanKey] = struct{}{}
			if !alreadyDemoted {
				prepared.demotedDatasets++
				prepared.redactionDemotions++
			}
		}
		prepared.frame.Datasets[cleanKey] = dataset
	}
	// Redaction can expand payloads, metadata and keys. Bound that exact copied
	// representation once; generated gap metadata needs no second redaction.
	redacted := prepared.frame.Datasets
	var redactedDemotions int64
	prepared.frame, redactedDemotions = boundReplayFrame(prepared.frame)
	if redactedDemotions != 0 {
		for key, before := range redacted {
			after, exists := prepared.frame.Datasets[key]
			if redactedDemotions != int64(len(redacted)) && exists &&
				len(before.Data) == len(after.Data) && bytes.Equal(before.Meta, after.Meta) {
				continue
			}
			if _, counted := demoted[key]; !counted {
				prepared.demotedDatasets++
			}
		}
	}
	return prepared, nil
}

func (s *Store) recordReplayFrame(ctx context.Context, frame ReplayFrame) (ReplayFrameWrite, error) {
	prepared, err := s.prepareReplayFrame(ctx, frame)
	if err != nil {
		return ReplayFrameWrite{}, err
	}
	frame = prepared.frame
	var captureID [16]byte
	if _, err := rand.Read(captureID[:]); err != nil {
		return ReplayFrameWrite{}, err
	}
	row := replayFrameRow{
		CaptureID: hex.EncodeToString(captureID[:]), AccountID: frame.AccountID,
		Generation: frame.Generation, Source: frame.Source,
		ObservedAt: frame.ObservedAt.UTC().Truncate(time.Microsecond), Clock: frame.Clock, Phase: frame.Phase,
		CatalogDigest: frame.CatalogDigest, Decision: frame.Decision, AssumedCatalog: frame.AssumedCatalog,
	}
	manifest := make(map[string]replayManifestEntry, len(frame.Datasets))
	payloads := make(map[string]replayPayloadInput)
	for key, dataset := range frame.Datasets {
		entry := replayManifestEntry{Meta: dataset.Meta}
		if len(dataset.Data) != 0 {
			originalKey := key
			if original, renamed := prepared.originalKeys[key]; renamed {
				originalKey = original
			}
			entry.Encoding = replayEncodingGzip
			if originalKey == "events" {
				// Event strings are attacker-influenced; never co-compress them
				// with private identifiers, even inside authenticated encryption.
				entry.Encoding = replayEncodingEvents
			}
			entry.Digest = s.replayEncodedDigest(frame.AccountID, entry.Encoding, dataset.Data)
			payloads[entry.Digest] = replayPayloadInput{Data: dataset.Data, Encoding: entry.Encoding}
			if originalKey == "catalog" {
				row.CatalogDigest = entry.Digest
			}
		}
		manifest[key] = entry
	}
	ciphertext, err := s.encryptReplayManifest(manifest, row)
	if err != nil {
		return ReplayFrameWrite{}, err
	}
	row.ManifestCipher, row.StoredBytes = ciphertext, int64(len(ciphertext))
	digests := make([]string, 0, len(payloads))
	for digest := range payloads {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	db, cancel := s.query(ctx)
	defer cancel()
	var added, compressedDemotions int64
	recorded := false
	err = db.Transaction(func(tx *gorm.DB) error {
		// Claim diagnostic generations before touching payloads. A conflicting
		// rerun introduces neither unused payloads nor changed evidence.
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		if result.Error != nil || result.RowsAffected == 0 {
			return result.Error
		}
		recorded = true
		if len(digests) == 0 {
			return nil
		}
		var existing []replayPayloadRow
		if err := tx.Select("digest").Clauses(clause.Locking{Strength: "KEY SHARE"}).
			Where("account_id = ? AND digest IN ?", frame.AccountID, digests).
			Order("digest").Find(&existing).Error; err != nil {
			return err
		}
		for _, payload := range existing {
			delete(payloads, payload.Digest)
		}
		oversize := false
		var missing []replayPayloadRow
		referenced := make([]string, 0, len(digests))
		for _, digest := range digests {
			input, absent := payloads[digest]
			if !absent {
				referenced = append(referenced, digest)
				continue
			}
			encoded := input.Data
			if input.Encoding == replayEncodingGzip {
				encoded, err = replayCompress(input.Data)
				if err != nil {
					return err
				}
			}
			if input.Encoding == replayEncodingGzip && len(encoded) > MaxReplayCompressedBytes {
				oversize = true
				for key, entry := range manifest {
					if entry.Digest != digest {
						continue
					}
					entry.Meta = replayDatasetGap(ReplayDataset{Data: input.Data, Meta: entry.Meta},
						replayCompressedCause, false).Meta
					entry.Digest, entry.Encoding = "", ""
					manifest[key] = entry
					compressedDemotions++
				}
				continue
			}
			ciphertext, err := s.encrypt(encoded, privateAAD("replay_payloads", "payload", frame.AccountID, digest))
			if err != nil {
				return err
			}
			missing = append(missing, replayPayloadRow{AccountID: frame.AccountID, Digest: digest,
				PayloadCipher: ciphertext, StoredBytes: int64(len(ciphertext)), LastSeenAt: row.ObservedAt})
			referenced = append(referenced, digest)
		}
		if len(missing) != 0 {
			result := tx.Clauses(clause.OnConflict{DoNothing: true},
				clause.Returning{Columns: []clause.Column{{Name: "digest"}}}).CreateInBatches(&missing, 128)
			if result.Error != nil {
				return result.Error
			}
			added += result.RowsAffected
			// ON CONFLICT can observe an insert absent from the first read.
			// Lock every surviving reference, including that concurrent insert.
			if err := tx.Select("digest").Clauses(clause.Locking{Strength: "KEY SHARE"}).
				Where("account_id = ? AND digest IN ?", frame.AccountID, referenced).
				Order("digest").Find(&existing).Error; err != nil {
				return err
			}
			if len(existing) != len(referenced) {
				return fmt.Errorf("replay payload disappeared during capture: %w", ErrNotFound)
			}
		}
		if oversize {
			ciphertext, err := s.encryptReplayManifest(manifest, row)
			if err != nil {
				return err
			}
			if err := tx.Model(&replayFrameRow{}).Where("id = ?", row.ID).Updates(map[string]any{
				"manifest_cipher": ciphertext, "stored_bytes": int64(len(ciphertext)),
			}).Error; err != nil {
				return err
			}
		}
		if len(referenced) == 0 {
			return nil
		}
		// Row locks, inserts and last-seen updates all use digest order, shared
		// with pruning. Coalescing avoids indexed non-HOT updates every pass.
		return tx.Exec(`WITH locked AS MATERIALIZED (
			SELECT digest FROM replay_payloads
			WHERE account_id = ? AND digest IN ? AND last_seen_at < ?
			ORDER BY digest FOR NO KEY UPDATE
		) UPDATE replay_payloads AS payload SET last_seen_at = ?
			FROM locked WHERE payload.account_id = ? AND payload.digest = locked.digest`,
			frame.AccountID, referenced, row.ObservedAt.Add(-replayLastSeenSlack), row.ObservedAt, frame.AccountID).Error
	})
	if err != nil {
		return ReplayFrameWrite{}, fmt.Errorf("record replay frame: %w", databaseError{cause: err})
	}
	if !recorded {
		return ReplayFrameWrite{}, nil
	}
	if frame.Source == "live" {
		s.observeReplayDemotions(frame.AccountID, replayFrameBudgetCause, prepared.demotedDatasets-prepared.redactionDemotions)
		s.observeReplayDemotions(frame.AccountID, replayRedactionSizeCause, prepared.redactionDemotions)
		s.observeReplayDemotions(frame.AccountID, replayCompressedCause, compressedDemotions)
	}
	return ReplayFrameWrite{ID: row.ID, PayloadsAdded: added,
		DemotedDatasets: prepared.demotedDatasets + compressedDemotions}, nil
}

func (s *Store) ScanReplayFrames(ctx context.Context, accountID string, yield func(ReplayFrame) error) error {
	previous := make(map[string][]byte)
	var after time.Time
	var afterID int64
	for {
		db, cancel := s.query(ctx)
		var rows []replayFrameRow
		err := db.Where("account_id = ? AND (observed_at, id) > (?, ?)", accountID, after, afterID).
			Order("observed_at, id").Limit(100).Find(&rows).Error
		cancel()
		if err != nil {
			return fmt.Errorf("scan replay frames: %w", databaseError{cause: err})
		}
		if len(rows) == 0 {
			return nil
		}
		for _, row := range rows {
			aad, err := replayManifestAAD(row)
			if err != nil {
				return err
			}
			compressed, err := s.decrypt(row.ManifestCipher, aad)
			if err != nil {
				return err
			}
			plain, err := replayDecompress(compressed)
			if err != nil {
				return err
			}
			var manifest map[string]replayManifestEntry
			if err := json.Unmarshal(plain, &manifest); err != nil {
				return err
			}
			frame := ReplayFrame{ID: row.ID, AccountID: accountID, Generation: row.Generation,
				Source: row.Source, ObservedAt: row.ObservedAt, Clock: row.Clock, Phase: row.Phase,
				CatalogDigest: row.CatalogDigest, Decision: row.Decision, AssumedCatalog: row.AssumedCatalog,
				Datasets: make(map[string]ReplayDataset, len(manifest))}
			current := make(map[string][]byte)
			for key, entry := range manifest {
				dataset := ReplayDataset{Meta: entry.Meta}
				if entry.Digest != "" {
					data, ok := current[entry.Digest]
					if !ok {
						data, ok = previous[entry.Digest]
					}
					if !ok {
						db, cancel := s.query(ctx)
						var payload replayPayloadRow
						err := db.Where("account_id = ? AND digest = ?", accountID, entry.Digest).First(&payload).Error
						cancel()
						if err != nil {
							return fmt.Errorf("resolve replay payload: %w", databaseError{cause: err})
						}
						compressed, err := s.decrypt(payload.PayloadCipher, privateAAD("replay_payloads", "payload", accountID, entry.Digest))
						if err != nil {
							return err
						}
						switch entry.Encoding {
						case replayEncodingGzip:
							data, err = replayDecompress(compressed)
						case replayEncodingEvents:
							if len(compressed) > MaxReplayPlaintextBytes {
								return ErrResponseTooLarge
							}
							data = compressed
						default:
							return fmt.Errorf("replay payload encoding: %w", ErrInvalidInput)
						}
						if err != nil {
							return err
						}
					}
					current[entry.Digest] = data
					dataset.Data = data
				}
				frame.Datasets[key] = dataset
			}
			if err := yield(frame); err != nil {
				return err
			}
			previous = current
			after, afterID = row.ObservedAt, row.ID
		}
	}
}

type replayPlanRow struct {
	ID         int64
	AccountID  string
	MissionID  string
	Generation string
	Sequence   int64
	ObservedAt time.Time
	Digest     string
	PlanCipher []byte
}

func (replayPlanRow) TableName() string { return "replay_plans" }

type replayPlanPrivate struct {
	ShipID   string
	Phase    string
	Plan     []byte
	Policy   []byte
	Revision string
}

func replayPlanTuple(plan ReplayPlan) replayPlanPrivate {
	return replayPlanPrivate{
		ShipID: plan.ShipID, Phase: plan.Phase, Plan: plan.Plan, Policy: plan.Policy, Revision: plan.Revision,
	}
}

// ReplayPlanFingerprint identifies the complete private tuple without I/O.
// Recorders must use this same tuple, not Plan alone, for in-memory deduplication.
func ReplayPlanFingerprint(plan ReplayPlan) [32]byte {
	plain, _ := json.Marshal(replayPlanTuple(plan))
	return sha256.Sum256(plain)
}

func replayPlanIdentity(row replayPlanRow) string {
	return privateFrame("v2", row.MissionID, row.Digest, row.Generation, strconv.FormatInt(row.Sequence, 10),
		replayTimestamp(row.ObservedAt))
}

func (s *Store) RecordReplayPlan(ctx context.Context, plan ReplayPlan) error {
	if !validID(plan.AccountID) || !validID(plan.MissionID) {
		return fmt.Errorf("replay plan fields: %w", ErrInvalidInput)
	}
	remaining := MaxReplayPlaintextBytes
	for _, size := range []int{len(plan.ShipID), len(plan.Phase), len(plan.Plan), len(plan.Policy), len(plan.Revision), len(plan.Generation)} {
		if size > remaining {
			return ErrResponseTooLarge
		}
		remaining -= size
	}
	if !json.Valid(plan.Plan) || !json.Valid(plan.Policy) {
		return fmt.Errorf("replay plan JSON: %w", ErrInvalidInput)
	}
	secrets, err := s.secrets(ctx, plan.AccountID)
	if err != nil {
		return fmt.Errorf("read replay credentials: %w", databaseError{cause: err})
	}
	plan.Plan, err = replayRedactJSON(plan.Plan, secrets)
	if err != nil {
		return err
	}
	plan.Policy, err = replayRedactJSON(plan.Policy, secrets)
	if err != nil {
		return err
	}
	plan.ShipID = scrubText(plan.ShipID, secrets)
	plan.Phase = scrubText(plan.Phase, secrets)
	plan.Revision = scrubText(plan.Revision, secrets)
	plain, err := json.Marshal(replayPlanTuple(plan))
	if err != nil {
		return err
	}
	if len(plain) > MaxReplayPlaintextBytes {
		return ErrResponseTooLarge
	}
	digest := s.replayEncodedDigest(plan.AccountID, "plan-json", plain)
	row := replayPlanRow{AccountID: plan.AccountID, MissionID: plan.MissionID, Generation: scrubText(plan.Generation, secrets),
		Sequence: plan.Sequence, ObservedAt: plan.ObservedAt.UTC().Truncate(time.Microsecond), Digest: digest}
	ciphertext, err := s.encrypt(plain, privateAAD("replay_plans", "plan", row.AccountID, replayPlanIdentity(row)))
	if err != nil {
		return err
	}
	row.PlanCipher = ciphertext
	db, cancel := s.query(ctx)
	defer cancel()
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
		return fmt.Errorf("record replay plan: %w", databaseError{cause: err})
	}
	return nil
}

func (s *Store) ReplayPlans(ctx context.Context, accountID string) ([]ReplayPlan, error) {
	db, cancel := s.query(ctx)
	defer cancel()
	var rows []replayPlanRow
	if err := db.Where("account_id = ?", accountID).Order("observed_at, id").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("read replay plans: %w", databaseError{cause: err})
	}
	plans := make([]ReplayPlan, 0, len(rows))
	for _, row := range rows {
		plain, err := s.decrypt(row.PlanCipher, privateAAD("replay_plans", "plan", row.AccountID, replayPlanIdentity(row)))
		if err != nil {
			return nil, err
		}
		if len(plain) > MaxReplayPlaintextBytes {
			return nil, ErrResponseTooLarge
		}
		var private replayPlanPrivate
		if err := json.Unmarshal(plain, &private); err != nil {
			return nil, err
		}
		plans = append(plans, ReplayPlan{ID: row.ID, AccountID: accountID, MissionID: row.MissionID,
			Generation: row.Generation, Sequence: row.Sequence, ObservedAt: row.ObservedAt,
			ShipID: private.ShipID, Phase: private.Phase, Plan: private.Plan, Policy: private.Policy, Revision: private.Revision})
	}
	return plans, nil
}

func (s *Store) LinkReplayAction(ctx context.Context, link ReplayActionLink) error {
	if !validID(link.AccountID) || !validID(link.ActionID) || !validID(link.MissionID) || !validID(link.Stage) {
		return fmt.Errorf("replay action link fields: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&link).Error; err != nil {
		return fmt.Errorf("link replay action: %w", databaseError{cause: err})
	}
	return nil
}

func (s *Store) ReplayActionLinks(ctx context.Context, accountID string) ([]ReplayActionLink, error) {
	db, cancel := s.query(ctx)
	defer cancel()
	var links []ReplayActionLink
	if err := db.Where("account_id = ?", accountID).Order("mission_id, sequence, action_id").Find(&links).Error; err != nil {
		return nil, fmt.Errorf("read replay action links: %w", databaseError{cause: err})
	}
	return links, nil
}

func (s *Store) PruneReplay(ctx context.Context, retention time.Duration, capBytes int64) (time.Time, error) {
	if retention <= 0 {
		return time.Time{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cutoff := time.Now().UTC().Add(-retention)
	pruneBefore := func(cutoff time.Time) error {
		for _, statement := range []struct {
			query  string
			cutoff time.Time
		}{
			{`WITH locked AS MATERIALIZED (
				SELECT id FROM replay_frames WHERE observed_at < ?
				ORDER BY observed_at, id LIMIT 10000 FOR UPDATE
			) DELETE FROM replay_frames AS frame USING locked
				WHERE frame.id = locked.id AND frame.observed_at < ?`, cutoff},
			{`WITH candidates AS MATERIALIZED (
				SELECT account_id, digest FROM replay_payloads WHERE last_seen_at < ?
				ORDER BY last_seen_at, account_id, digest LIMIT 10000
			), locked AS MATERIALIZED (
				SELECT payload.account_id, payload.digest FROM replay_payloads AS payload
				JOIN candidates USING (account_id, digest)
				ORDER BY payload.account_id, payload.digest FOR UPDATE OF payload
			) DELETE FROM replay_payloads AS payload USING locked
				WHERE payload.account_id = locked.account_id AND payload.digest = locked.digest AND payload.last_seen_at < ?`,
				cutoff.Add(-replayLastSeenSlack)},
		} {
			for {
				db, cancel := s.query(ctx)
				result := db.Exec(statement.query, statement.cutoff, statement.cutoff)
				cancel()
				if result.Error != nil {
					return fmt.Errorf("prune replay: %w", databaseError{cause: result.Error})
				}
				if result.RowsAffected < 10000 {
					break
				}
			}
		}
		return nil
	}
	if err := pruneBefore(cutoff); err != nil {
		return cutoff, err
	}
	if capBytes <= 0 {
		return cutoff, nil
	}
	db, queryCancel := s.query(ctx)
	var boundary struct{ At *time.Time }
	err := db.Raw(`WITH sizes AS (
		SELECT observed_at AS at, stored_bytes FROM replay_frames
		UNION ALL SELECT last_seen_at + interval '1 hour' AS at, stored_bytes FROM replay_payloads
	), cumulative AS (
		SELECT at, SUM(SUM(stored_bytes)) OVER (ORDER BY at DESC) AS bytes
		FROM sizes GROUP BY at
	) SELECT max(at) AS at FROM cumulative WHERE bytes > ?`, capBytes).Scan(&boundary).Error
	queryCancel()
	if err != nil {
		return cutoff, fmt.Errorf("measure replay cap: %w", databaseError{cause: err})
	}
	if boundary.At != nil {
		if capCutoff := boundary.At.Add(time.Microsecond); capCutoff.After(cutoff) {
			cutoff = capCutoff
		}
		if err := pruneBefore(cutoff); err != nil {
			return cutoff, err
		}
	}
	return cutoff, nil
}
