package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type BulletinThread struct {
	ID          string
	Title       string
	CompanyName string
	Link        json.RawMessage
	CreatedAt   time.Time
	LastPostAt  time.Time
	Replies     int
	Archived    bool
	Removed     bool
}

type BulletinPost struct {
	ID            string
	ThreadID      string
	CompanyName   string
	Body          string
	IsOpening     bool
	CreatedAt     time.Time
	EditedAt      *time.Time
	EditableUntil *time.Time
	Removed       bool
	Deleted       bool
}

type BulletinQuery struct {
	Search  string
	Company string
	Limit   int
}

type BulletinThreadSummary struct {
	ID             string          `json:"id"`
	Title          string          `json:"title"`
	CompanyName    string          `json:"company_name"`
	Link           json.RawMessage `json:"link" gorm:"type:jsonb"`
	CreatedAt      time.Time       `json:"created_at"`
	LastPostAt     time.Time       `json:"last_post_at"`
	Replies        int             `json:"replies"`
	PostsRecorded  int             `json:"posts_recorded"`
	Archived       bool            `json:"archived"`
	Removed        bool            `json:"removed"`
	DetailSyncedAt *time.Time      `json:"detail_synced_at"`
	DetailCurrent  bool            `json:"detail_current"`
}

type BulletinBoard struct {
	Threads            []BulletinThreadSummary `json:"threads"`
	Total              int                     `json:"total"`
	Companies          []string                `json:"companies"`
	SyncedAt           *time.Time              `json:"synced_at"`
	CompaniesTruncated bool                    `json:"companies_truncated"`
}

type BulletinPostView struct {
	ID          string     `json:"id"`
	CompanyName string     `json:"company_name"`
	Body        string     `json:"body"`
	IsOpening   bool       `json:"is_opening"`
	CreatedAt   time.Time  `json:"created_at"`
	EditedAt    *time.Time `json:"edited_at"`
	Removed     bool       `json:"removed"`
	Deleted     bool       `json:"deleted"`
}

type BulletinThreadView struct {
	Thread         BulletinThreadSummary `json:"thread"`
	Posts          []BulletinPostView    `json:"posts"`
	PostsTotal     int                   `json:"posts_total"`
	PostsTruncated bool                  `json:"posts_truncated"`
}

func bulletinText(text string, limit int) string {
	text = strings.Map(func(value rune) rune {
		if value < 32 && value != '\n' && value != '\t' {
			return -1
		}
		return value
	}, text)
	count := 0
	for index := range text {
		if count == limit {
			return text[:index]
		}
		count++
	}
	return text
}

func bulletinLink(raw json.RawMessage) any {
	if len(raw) == 0 || len(raw) >= 1024 || !utf8.Valid(raw) || !json.Valid(raw) || !bytes.Equal(BoundEventPayload(raw), raw) || !bulletinJSONStringsSafe(raw) {
		return nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return nil
	}
	return string(raw)
}

// Go replaces malformed UTF-16 escapes while PostgreSQL rejects them.
func bulletinJSONStringsSafe(raw []byte) bool {
	for index := 0; index < len(raw); index++ {
		if raw[index] != '"' {
			continue
		}
		for index++; index < len(raw) && raw[index] != '"'; index++ {
			if raw[index] != '\\' {
				continue
			}
			index++
			if raw[index] != 'u' {
				continue
			}
			value, err := strconv.ParseUint(string(raw[index+1:index+5]), 16, 16)
			if err != nil || value == 0 || value >= 0xDC00 && value <= 0xDFFF {
				return false
			}
			index += 4
			if value >= 0xD800 && value <= 0xDBFF {
				if index+7 > len(raw) || raw[index+1] != '\\' || raw[index+2] != 'u' {
					return false
				}
				low, err := strconv.ParseUint(string(raw[index+3:index+7]), 16, 16)
				if err != nil || low < 0xDC00 || low > 0xDFFF {
					return false
				}
				index += 6
			}
		}
	}
	return true
}

func saveBulletinThread(tx *gorm.DB, thread BulletinThread, now time.Time, listing bool) error {
	if uuid.Validate(thread.ID) != nil || thread.Replies < 0 {
		return fmt.Errorf("bulletin thread: %w", ErrInvalidInput)
	}
	guard := ""
	if listing {
		guard = " WHERE (bulletin_threads.listed_at IS NULL AND bulletin_threads.detail_synced_at IS NULL) OR EXCLUDED.last_post_at >= bulletin_threads.last_post_at"
	}
	return tx.Exec(`INSERT INTO bulletin_threads
		(id, title, company_name, link, created_at, last_post_at, replies, archived, removed, first_seen_at, updated_at, listed_at, invalidated_at)
		VALUES (?, ?, ?, ?::jsonb, ?, ?, ?, ?, ?, ?, ?, now(), now())
		ON CONFLICT (id) DO UPDATE SET title = EXCLUDED.title, company_name = EXCLUDED.company_name,
		link = EXCLUDED.link, created_at = EXCLUDED.created_at,
		last_post_at = EXCLUDED.last_post_at,
		replies = EXCLUDED.replies, archived = EXCLUDED.archived, removed = EXCLUDED.removed,
		listed_at = now(),
		invalidated_at = CASE WHEN ? AND (EXCLUDED.last_post_at > bulletin_threads.last_post_at
			OR EXCLUDED.replies <> bulletin_threads.replies OR EXCLUDED.title IS DISTINCT FROM bulletin_threads.title
			OR EXCLUDED.company_name IS DISTINCT FROM bulletin_threads.company_name
			OR EXCLUDED.link IS DISTINCT FROM bulletin_threads.link
			OR EXCLUDED.archived <> bulletin_threads.archived OR EXCLUDED.removed <> bulletin_threads.removed)
			THEN now() ELSE bulletin_threads.invalidated_at END,
		updated_at = EXCLUDED.updated_at`+guard, thread.ID, bulletinText(thread.Title, 300),
		bulletinText(thread.CompanyName, 120), bulletinLink(thread.Link), thread.CreatedAt,
		thread.LastPostAt, thread.Replies, thread.Archived, thread.Removed, now, now, listing).Error
}

func saveBulletinPosts(tx *gorm.DB, threadID string, posts []BulletinPost, now time.Time) error {
	type postRow struct {
		BulletinPost `gorm:"embedded"`
		FirstSeenAt  time.Time
		UpdatedAt    time.Time
	}
	rows := make([]postRow, 0, len(posts))
	for _, post := range posts {
		if uuid.Validate(post.ID) != nil || post.ThreadID != threadID {
			return fmt.Errorf("bulletin post: %w", ErrInvalidInput)
		}
		post.CompanyName = bulletinText(post.CompanyName, 120)
		post.Body = bulletinText(post.Body, 8000)
		rows = append(rows, postRow{BulletinPost: post, FirstSeenAt: now, UpdatedAt: now})
	}
	if len(rows) == 0 {
		return nil
	}
	// Twelve bind parameters per row; batches stay well under PostgreSQL's
	// 65,535-parameter statement limit for threads with thousands of replies.
	return tx.Table("bulletin_posts").Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"thread_id", "company_name", "body", "is_opening",
			"created_at", "edited_at", "editable_until", "removed", "deleted", "updated_at"}),
	}).CreateInBatches(&rows, 1000).Error
}

func (s *Store) SaveBulletinThreads(ctx context.Context, accountID string, threads []BulletinThread) error {
	threads = slices.Clone(threads)
	slices.SortFunc(threads, func(a, b BulletinThread) int { return strings.Compare(a.ID, b.ID) })
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		for _, thread := range threads {
			if err := saveBulletinThread(tx, thread, now, true); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) SaveBulletinThreadDetail(ctx context.Context, accountID string, thread BulletinThread, posts []BulletinPost) error {
	posts = slices.Clone(posts)
	slices.SortFunc(posts, func(a, b BulletinPost) int { return strings.Compare(a.ID, b.ID) })
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		if err := saveBulletinThread(tx, thread, now, false); err != nil {
			return err
		}
		if err := saveBulletinPosts(tx, thread.ID, posts, now); err != nil {
			return err
		}
		return tx.Exec(`UPDATE bulletin_threads SET
			detail_last_post_at = CASE WHEN invalidated_at > detail_attempted_at THEN NULL ELSE ?::timestamptz END,
			detail_synced_at = COALESCE(detail_attempted_at, now()), detail_replies = ?, detail_attempts = 0 WHERE id = ?`,
			thread.LastPostAt, thread.Replies, thread.ID).Error
	})
}

func (s *Store) SaveBulletinEventPost(ctx context.Context, accountID, threadTitle string, post BulletinPost) error {
	if uuid.Validate(post.ThreadID) != nil || uuid.Validate(post.ID) != nil {
		return fmt.Errorf("bulletin event: %w", ErrInvalidInput)
	}
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		company := ""
		if post.IsOpening {
			company = bulletinText(post.CompanyName, 120)
		}
		if err := tx.Exec(`INSERT INTO bulletin_threads
			(id, title, company_name, created_at, last_post_at, replies, archived, removed, first_seen_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 0, false, false, ?, ?)
			ON CONFLICT (id) DO NOTHING`,
			post.ThreadID, bulletinText(threadTitle, 300), company, post.CreatedAt, post.CreatedAt, now, now).Error; err != nil {
			return err
		}
		result := tx.Exec(`INSERT INTO bulletin_posts
			(id, thread_id, company_name, body, is_opening, created_at, edited_at, editable_until, removed, deleted, first_seen_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING RETURNING id`,
			post.ID, post.ThreadID, bulletinText(post.CompanyName, 120), bulletinText(post.Body, 8000),
			post.IsOpening, post.CreatedAt, post.EditedAt, post.EditableUntil, post.Removed, post.Deleted, now, now)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}
		return tx.Exec(`UPDATE bulletin_threads SET detail_last_post_at = NULL, invalidated_at = now(), updated_at = now() WHERE id = ?`, post.ThreadID).Error
	})
}

func (s *Store) BulletinThreadsNeedingDetail(ctx context.Context, limit int) ([]string, error) {
	if limit < 1 || limit > MaxHistoryLimit {
		return nil, fmt.Errorf("bulletin detail limit: %w", ErrInvalidInput)
	}
	ids := make([]string, 0)
	db, cancel := s.query(ctx)
	defer cancel()
	err := db.Transaction(func(tx *gorm.DB) error {
		return tx.Table("bulletin_threads t").Where("("+bulletinDetailStale+") AND "+bulletinDetailReady).
			Order(bulletinDetailOrder).Limit(limit).Pluck("id", &ids).Error
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return ids, err
}

const bulletinActivity = `GREATEST(t.last_post_at, COALESCE((SELECT max(p.created_at) FROM bulletin_posts p WHERE p.thread_id = t.id), t.last_post_at))`
const bulletinArchived = `(t.archived OR ` + bulletinActivity + ` <= now() - interval '72 hours')`
const bulletinDetailStale = `t.detail_synced_at IS NULL OR t.detail_last_post_at IS NULL
	OR COALESCE(t.invalidated_at > t.detail_synced_at, false)
	OR t.detail_last_post_at < t.last_post_at OR t.detail_replies IS DISTINCT FROM t.replies
	OR EXISTS (SELECT 1 FROM bulletin_posts p WHERE p.thread_id = t.id
		AND p.editable_until > t.detail_synced_at AND p.editable_until <= now())
	OR (NOT ` + bulletinArchived + ` AND t.removed = false AND t.detail_synced_at < now() - interval '6 hours')`
const bulletinDetailReady = `(t.detail_attempted_at IS NULL OR t.invalidated_at > t.detail_attempted_at
	OR t.detail_attempted_at <= now() - interval '10 minutes' * LEAST(36, power(2, LEAST(GREATEST(t.detail_attempts - 1, 0), 6))))`
const bulletinDetailOrder = `CASE WHEN t.detail_synced_at IS NOT NULL AND (t.detail_last_post_at IS NULL
	OR t.invalidated_at > t.detail_synced_at
	OR t.detail_last_post_at < t.last_post_at OR t.detail_replies IS DISTINCT FROM t.replies) THEN 0
	WHEN t.detail_attempted_at IS NULL THEN 1 ELSE 2 END,
	t.detail_attempted_at ASC NULLS FIRST, t.last_post_at DESC, t.id`

const bulletinSummarySelect = `t.id, t.title, t.company_name, t.link, t.created_at, t.last_post_at,
	t.replies, ` + bulletinArchived + ` AS archived, t.removed, t.detail_synced_at,
	(SELECT count(*) FROM bulletin_posts p WHERE p.thread_id = t.id) AS posts_recorded,
	NOT (` + bulletinDetailStale + `) AS detail_current`

func (s *Store) BulletinBoard(ctx context.Context, query BulletinQuery) (BulletinBoard, error) {
	if query.Limit == 0 {
		query.Limit = 200
	}
	if query.Limit < 1 || query.Limit > MaxHistoryLimit || utf8.RuneCountInString(query.Search) > 200 {
		return BulletinBoard{}, fmt.Errorf("bulletin query: %w", ErrInvalidInput)
	}
	board := BulletinBoard{Threads: make([]BulletinThreadSummary, 0), Companies: make([]string, 0)}
	db, cancel := s.query(ctx)
	defer cancel()
	err := db.Transaction(func(tx *gorm.DB) error {
		filter := tx.Table("bulletin_threads t")
		if query.Search != "" {
			search := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query.Search) + "%"
			filter = filter.Where(`t.title ILIKE ? ESCAPE '\' OR t.company_name ILIKE ? ESCAPE '\' OR EXISTS
				(SELECT 1 FROM bulletin_posts p WHERE p.thread_id = t.id
				AND (p.body ILIKE ? ESCAPE '\' OR p.company_name ILIKE ? ESCAPE '\'))`, search, search, search, search)
		}
		if query.Company != "" {
			filter = filter.Where(`t.company_name = ? OR EXISTS (SELECT 1 FROM bulletin_posts p WHERE p.thread_id = t.id AND p.company_name = ?)`, query.Company, query.Company)
		}
		var total int64
		if err := filter.Session(&gorm.Session{}).Count(&total).Error; err != nil {
			return err
		}
		board.Total = int(total)
		if err := filter.Select(bulletinSummarySelect).Order("t.last_post_at DESC, t.id").Limit(query.Limit).Scan(&board.Threads).Error; err != nil {
			return err
		}
		if err := tx.Raw(`SELECT company_name FROM (SELECT company_name FROM bulletin_threads UNION SELECT company_name FROM bulletin_posts) c
			WHERE company_name <> '' ORDER BY company_name LIMIT 1001`).Scan(&board.Companies).Error; err != nil {
			return err
		}
		if len(board.Companies) > 1000 {
			board.Companies = board.Companies[:1000]
			board.CompaniesTruncated = true
		}
		var freshness struct{ SyncedAt *time.Time }
		if err := tx.Raw(`SELECT synced_at FROM bulletin_sync WHERE singleton`).Scan(&freshness).Error; err != nil {
			return err
		}
		board.SyncedAt = freshness.SyncedAt
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return board, err
}

// MaxBulletinThreadBytes bounds the encoded dashboard response, including JSON
// escaping. Streaming posts avoids hydrating the entire recorded history.
const MaxBulletinThreadBytes = 8 * 1024 * 1024

func (s *Store) BulletinThread(ctx context.Context, id string) (BulletinThreadView, error) {
	if uuid.Validate(id) != nil {
		return BulletinThreadView{}, fmt.Errorf("bulletin thread id: %w", ErrInvalidInput)
	}
	view := BulletinThreadView{Posts: make([]BulletinPostView, 0)}
	db, cancel := s.query(ctx)
	defer cancel()
	err := db.Transaction(func(tx *gorm.DB) error {
		result := tx.Table("bulletin_threads t").Select(bulletinSummarySelect).Where("t.id = ?", id).Scan(&view.Thread)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		view.PostsTotal = view.Thread.PostsRecorded
		header, err := json.Marshal(view)
		if err != nil {
			return err
		}
		remaining := MaxBulletinThreadBytes - len(header)
		rows, err := tx.Table("bulletin_posts").
			Select("id, company_name, body, is_opening, created_at, edited_at, removed, deleted").
			Where("thread_id = ?", id).Order("created_at DESC, id DESC").Limit(1000).Rows()
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var post BulletinPostView
			if err := tx.ScanRows(rows, &post); err != nil {
				return err
			}
			encoded, err := json.Marshal(post)
			if err != nil {
				return err
			}
			size := len(encoded)
			if len(view.Posts) > 0 {
				size++ // Array separator.
			}
			if size > remaining {
				break
			}
			remaining -= size
			view.Posts = append(view.Posts, post)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		slices.Reverse(view.Posts)
		view.PostsTruncated = view.PostsTotal > len(view.Posts)
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return view, err
}

type BulletinListClaim struct {
	Claimed      bool
	ListCursor   string
	BackfillDone bool
	HeadCursor   string
	HeadSince    time.Time
}

// ClaimBulletinList shares read cadence and independent head/backfill cursors.
func (s *Store) ClaimBulletinList(ctx context.Context, accountID string) (BulletinListClaim, error) {
	var state BulletinListClaim
	err := s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		result := tx.Raw(`UPDATE bulletin_sync SET listed_at = now(),
			head_since = CASE WHEN head_started_at IS NULL THEN COALESCE(synced_at, now()) - interval '2 minutes' ELSE head_since END,
			head_started_at = COALESCE(head_started_at, now())
			WHERE singleton AND (listed_at IS NULL OR listed_at < now() - interval '10 minutes')
			RETURNING COALESCE(list_cursor, '') AS list_cursor, backfill_done,
			COALESCE(head_cursor, '') AS head_cursor, head_since`).Scan(&state)
		state.Claimed = result.RowsAffected == 1
		return result.Error
	})
	return state, err
}

func (s *Store) SaveBulletinListProgress(ctx context.Context, accountID, cursor string, backfillDone bool) error {
	if len(cursor) > 4096 {
		return fmt.Errorf("bulletin list cursor: %w", ErrInvalidInput)
	}
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		return tx.Exec(`UPDATE bulletin_sync SET list_cursor = NULLIF(?, ''), backfill_done = ? WHERE singleton`,
			cursor, backfillDone).Error
	})
}

func (s *Store) SaveBulletinHeadProgress(ctx context.Context, accountID, cursor string) error {
	if len(cursor) > 4096 {
		return fmt.Errorf("bulletin head cursor: %w", ErrInvalidInput)
	}
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		return tx.Exec(`UPDATE bulletin_sync SET head_cursor = NULLIF(?, ''),
			synced_at = CASE WHEN ? = '' THEN head_started_at ELSE synced_at END,
			head_since = CASE WHEN ? = '' THEN NULL ELSE head_since END,
			head_started_at = CASE WHEN ? = '' THEN NULL ELSE head_started_at END WHERE singleton`,
			cursor, cursor, cursor, cursor).Error
	})
}

func (s *Store) ResetBulletinListCursor(ctx context.Context, accountID string, head bool) error {
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		if head {
			return tx.Exec(`UPDATE bulletin_sync SET head_cursor = NULL WHERE singleton`).Error
		}
		return tx.Exec(`UPDATE bulletin_sync SET list_cursor = NULL WHERE singleton`).Error
	})
}

func (s *Store) ClaimBulletinDetails(ctx context.Context, accountID string, limit int) ([]string, error) {
	if limit < 1 || limit > MaxHistoryLimit {
		return nil, fmt.Errorf("bulletin detail limit: %w", ErrInvalidInput)
	}
	ids := make([]string, 0)
	err := s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		return tx.Raw(`WITH candidates AS (
			SELECT t.id FROM bulletin_threads t WHERE (`+bulletinDetailStale+`) AND `+bulletinDetailReady+`
			ORDER BY `+bulletinDetailOrder+` LIMIT ? FOR UPDATE OF t SKIP LOCKED
		) UPDATE bulletin_threads t SET detail_attempted_at = now(), detail_attempts = detail_attempts + 1
		FROM candidates c WHERE t.id = c.id RETURNING t.id`, limit).Scan(&ids).Error
	})
	return ids, err
}

func (s *Store) ReleaseBulletinDetailClaims(ctx context.Context, accountID string, ids []string) error {
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		return tx.Exec(`UPDATE bulletin_threads SET detail_attempted_at = NULL,
			detail_attempts = GREATEST(detail_attempts - 1, 0) WHERE id IN ?`, ids).Error
	})
}

func (s *Store) MarkBulletinThreadMissing(ctx context.Context, accountID, id string) error {
	if uuid.Validate(id) != nil {
		return fmt.Errorf("bulletin thread id: %w", ErrInvalidInput)
	}
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		return tx.Exec(`UPDATE bulletin_threads SET detail_synced_at = COALESCE(detail_attempted_at, now()),
			detail_last_post_at = last_post_at, detail_replies = replies, detail_attempts = 0, updated_at = now() WHERE id = ?`, id).Error
	})
}
