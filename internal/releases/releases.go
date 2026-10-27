package releases

import (
	"context"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"go.uber.org/zap"
)

// Unseen returns only the prefix before a known cursor in the newest-first
// feed. A missing cursor is not evidence that historical notes are new.
func Unseen(lastSeen string, notes []gameapi.ReleaseNote) []gameapi.ReleaseNote {
	if lastSeen == "" {
		return nil
	}
	for index, note := range notes {
		if note.Version == lastSeen {
			return notes[:index]
		}
	}
	return nil
}

// Watch polls independently of account workers. Its first successful response,
// even an empty one, establishes the baseline without announcing old releases.
func Watch(ctx context.Context, fetch func(context.Context) ([]gameapi.ReleaseNote, error), log *zap.Logger, interval time.Duration) {
	var lastSeen string
	baselined, failed := false, false
	retry := min(interval, time.Minute)
	for ctx.Err() == nil {
		fetchCtx, cancel := context.WithTimeout(ctx, time.Minute)
		notes, err := fetch(fetchCtx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		delay := interval
		if err != nil {
			if !failed {
				log.Warn("game release notes unavailable", zap.Error(err))
			}
			failed = true
			delay = retry/2 + time.Duration(rand.Int64N(int64(retry-retry/2)+1))
			retry = min(2*retry, time.Hour)
		} else {
			failed = false
			retry = min(interval, time.Minute)
			if !baselined {
				baselined = true
				if len(notes) > 0 {
					log.Info("game release notes", releaseFields(notes[0])...)
					lastSeen = notes[0].Version
				}
			} else {
				unseen := Unseen(lastSeen, notes)
				if lastSeen == "" {
					unseen = notes
				}
				for _, note := range unseen[:min(len(unseen), 5)] {
					fields := releaseFields(note)
					fields = append(fields, zap.Strings("headings", releaseHeadings(note.Markdown)))
					log.Warn("game updated while the bot is running", fields...)
				}
				if len(unseen) > 0 {
					lastSeen = unseen[0].Version
				}
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func releaseFields(note gameapi.ReleaseNote) []zap.Field {
	return []zap.Field{
		zap.String("version", boundedText(note.Version, 64)),
		zap.String("released_on", boundedText(note.ReleasedOn, 64)),
	}
}

func boundedText(text string, limit int) string {
	text = gameapi.RedactPublicText(text)
	count := 0
	for index := range text {
		if count == limit {
			return text[:index]
		}
		count++
	}
	return text
}

func releaseHeadings(markdown string) []string {
	var headings []string
	var fence byte
	width := 0
	for line := range strings.Lines(markdown) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			run := 0
			for run < len(line) && line[run] == line[0] {
				run++
			}
			if fence == 0 {
				fence, width = line[0], run
			} else if line[0] == fence && run >= width && strings.TrimSpace(line[run:]) == "" {
				fence = 0
			}
			continue
		}
		if fence == 0 && strings.HasPrefix(line, "#") {
			headings = append(headings, boundedText(line, 200))
			if len(headings) == 6 {
				break
			}
		}
	}
	return headings
}
