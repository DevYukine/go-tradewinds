package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	bulletinMirrorBudget      = 3 * time.Second
	bulletinMirrorWriteMargin = time.Second
)

func (r *Runner) recordBulletinEvent(ctx context.Context, raw json.RawMessage) {
	var event struct {
		ThreadID    string    `json:"thread_id"`
		PostID      string    `json:"post_id"`
		Title       string    `json:"title"`
		CompanyName string    `json:"company_name"`
		Body        string    `json:"body"`
		IsReply     bool      `json:"is_reply"`
		Timestamp   time.Time `json:"timestamp"`
	}
	timestamp, ok := decodeEventPayload(raw, &event)
	if !ok {
		r.log.Warn("invalid bulletin event ignored")
		return
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = timestamp
	}
	if uuid.Validate(event.ThreadID) != nil || uuid.Validate(event.PostID) != nil || event.Timestamp.IsZero() {
		r.log.Warn("incomplete bulletin event ignored")
		return
	}
	err := r.db.SaveBulletinEventPost(ctx, r.account.ID, event.Title, store.BulletinPost{
		ID: event.PostID, ThreadID: event.ThreadID, CompanyName: event.CompanyName,
		Body: event.Body, IsOpening: !event.IsReply, CreatedAt: event.Timestamp,
	})
	if err != nil {
		r.log.Warn("bulletin event mirror write failed", zap.Error(err))
	}
}

func (r *Runner) bulletinMirrorError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrExecutionStopped) || errors.Is(err, store.ErrOwnershipLost) {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Optional diagnostics cannot become fleet holds. Ownership loss and
	// parent cancellation still belong to the runner's execution boundary.
	if decisionErr := r.decision(ctx, "__bulletin_mirror", "bulletin_mirror", "bulletin mirror deferred; trading remains independent", map[string]string{"error": err.Error()}); decisionErr != nil {
		if errors.Is(decisionErr, store.ErrOwnershipLost) {
			return decisionErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.log.Warn("record bulletin mirror diagnostic failed", zap.Error(decisionErr))
	}
	return nil
}

func mirroredBulletinThread(thread bulletinThread) store.BulletinThread {
	return store.BulletinThread{
		ID: thread.ID, Title: thread.Title, CompanyName: thread.CompanyName,
		Link: thread.Link, CreatedAt: thread.CreatedAt, LastPostAt: thread.LastPostAt,
		Replies: thread.Replies, Archived: thread.Archived, Removed: thread.Removed,
	}
}

func (r *Runner) syncBulletinMirror(ctx context.Context) (err error) {
	window := bulletinMirrorBudget + bulletinMirrorWriteMargin + bulletinRequestBudget
	if r.nextWake() <= window || !r.nextDeadline.IsZero() && time.Until(r.nextDeadline) <= window {
		return nil
	}
	mirrorCtx, cancel := context.WithTimeout(ctx, bulletinMirrorBudget)
	defer cancel()
	defer func() {
		if errors.Is(err, ErrExecutionStopped) || errors.Is(err, store.ErrOwnershipLost) {
			return
		}
		if ctx.Err() != nil {
			err = ctx.Err()
			return
		}
		if mirrorCtx.Err() != nil {
			if err != nil {
				r.log.Warn("bulletin mirror budget exhausted", zap.Error(err))
			}
			err = nil
			return
		}
		err = r.bulletinMirrorError(mirrorCtx, err)
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			err = nil
		}
	}()
	requestCtx, requestCancel := context.WithTimeout(mirrorCtx, bulletinMirrorBudget-bulletinMirrorWriteMargin)
	defer requestCancel()
	claim, err := r.db.ClaimBulletinList(mirrorCtx, r.account.ID)
	if err != nil {
		return err
	}
	if claim.Claimed {
		head := true
		cursor := claim.HeadCursor
		for range 5 {
			path := "/v1/bulletin?limit=100"
			if head {
				path += "&since=" + url.QueryEscape(claim.HeadSince.Format(time.RFC3339Nano))
			}
			if cursor != "" {
				path += "&cursor=" + url.QueryEscape(cursor)
			}
			var page bulletinPage
			readErr := r.api.GetOnce(requestCtx, path, &page)
			if readErr != nil {
				if fatal := bulletinMirrorAuthorization(ctx, readErr); fatal != nil {
					return fatal
				}
				var upstream *gameapi.APIError
				if cursor != "" && errors.As(readErr, &upstream) &&
					(upstream.Status == http.StatusBadRequest || upstream.Status == http.StatusNotFound ||
						upstream.Status == http.StatusGone || upstream.Status == http.StatusUnprocessableEntity) {
					if resetErr := r.db.ResetBulletinListCursor(mirrorCtx, r.account.ID, head); resetErr != nil {
						return resetErr
					}
					return errors.Join(errors.New("bulletin mirror rejected cursor reset to the first page"), readErr)
				}
				return readErr
			}
			if page.Threads == nil || len(page.Threads) > bulletinPageLimit || len(page.NextCursor) > bulletinCursorLimit || page.NextCursor != "" && page.NextCursor == cursor {
				if cursor != "" {
					if resetErr := r.db.ResetBulletinListCursor(mirrorCtx, r.account.ID, head); resetErr != nil {
						return resetErr
					}
				}
				return errors.New("bulletin mirror invalid page")
			}
			threads := make([]store.BulletinThread, 0, len(page.Threads))
			for _, thread := range page.Threads {
				if uuid.Validate(thread.ID) != nil || thread.Replies < 0 {
					continue
				}
				threads = append(threads, mirroredBulletinThread(thread))
			}
			if saveErr := r.db.SaveBulletinThreads(mirrorCtx, r.account.ID, threads); saveErr != nil {
				return saveErr
			}
			cursor = page.NextCursor
			if head {
				if err := r.db.SaveBulletinHeadProgress(mirrorCtx, r.account.ID, cursor); err != nil {
					return err
				}
				if cursor == "" {
					if claim.BackfillDone {
						break
					}
					head, cursor = false, claim.ListCursor
				}
			} else {
				if err := r.db.SaveBulletinListProgress(mirrorCtx, r.account.ID, cursor, cursor == ""); err != nil {
					return err
				}
				if cursor == "" {
					break
				}
			}
		}
	}
	if requestCtx.Err() != nil {
		return requestCtx.Err()
	}
	ids, err := r.db.ClaimBulletinDetails(mirrorCtx, r.account.ID, 2)
	if err != nil {
		return err
	}
	// A request can consume the remaining window. Release any later claims on
	// every exit rather than counting requests we never sent as failed attempts.
	pending := ids
	defer func() {
		if len(pending) == 0 {
			return
		}
		releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), bulletinMirrorWriteMargin)
		defer releaseCancel()
		err = errors.Join(err, r.db.ReleaseBulletinDetailClaims(releaseCtx, r.account.ID, pending))
	}()
	var detailErrors error
	for _, id := range ids {
		if requestCtx.Err() != nil {
			return errors.Join(detailErrors, requestCtx.Err())
		}
		pending = pending[1:]
		var detail bulletinDetail
		readErr := r.api.GetOnce(requestCtx, "/v1/bulletin/thread?id="+url.QueryEscape(id), &detail)
		if fatal := bulletinMirrorAuthorization(ctx, readErr); fatal != nil {
			return fatal
		}
		var upstream *gameapi.APIError
		if errors.As(readErr, &upstream) && upstream.Status == http.StatusNotFound {
			if err := r.db.MarkBulletinThreadMissing(mirrorCtx, r.account.ID, id); err != nil {
				return err
			}
			continue
		}
		if readErr != nil {
			detailErrors = errors.Join(detailErrors, readErr)
			continue
		}
		if detail.Thread.ID != id || detail.Posts == nil {
			detailErrors = errors.Join(detailErrors, errors.New("bulletin mirror detail omitted the selected thread or posts"))
			continue
		}
		posts := make([]store.BulletinPost, 0, len(detail.Posts))
		for _, post := range detail.Posts {
			posts = append(posts, store.BulletinPost{
				ID: post.ID, ThreadID: post.ThreadID, CompanyName: post.CompanyName,
				Body: post.Body, IsOpening: post.Opening, CreatedAt: post.CreatedAt,
				EditedAt: post.EditedAt, Removed: post.Removed, Deleted: post.Deleted,
				EditableUntil: post.EditableUntil,
			})
		}
		if saveErr := r.db.SaveBulletinThreadDetail(mirrorCtx, r.account.ID, mirroredBulletinThread(detail.Thread), posts); saveErr != nil {
			if errors.Is(saveErr, store.ErrOwnershipLost) || ctx.Err() != nil {
				return saveErr
			}
			detailErrors = errors.Join(detailErrors, saveErr)
		}
	}
	return detailErrors
}

func bulletinMirrorAuthorization(ctx context.Context, err error) error {
	var upstream *gameapi.APIError
	if errors.As(err, &upstream) && gameapi.BulletinAuthorizationRefusal(upstream.Status, upstream.Body) {
		return bulletinAuthorizationError(ctx, upstream.Status, err)
	}
	return nil
}
