package releases

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestUnseen(t *testing.T) {
	notes := []gameapi.ReleaseNote{
		{Version: "0.4.2", ReleasedOn: "2026-10-28", Markdown: "# Third release"},
		{Version: "0.4.1", ReleasedOn: "2026-10-27", Markdown: "# Second release"},
		{Version: "0.4.0", ReleasedOn: "2026-10-26", Markdown: "# First release"},
	}
	for _, test := range []struct {
		name     string
		lastSeen string
		notes    []gameapi.ReleaseNote
		want     []gameapi.ReleaseNote
	}{
		{name: "startup", notes: notes},
		{name: "no notes", lastSeen: "0.4.0"},
		{name: "empty notes", lastSeen: "0.4.0", notes: []gameapi.ReleaseNote{}},
		{name: "already current", lastSeen: "0.4.2", notes: notes},
		{name: "one unseen", lastSeen: "0.4.1", notes: notes, want: notes[:1]},
		{name: "multiple unseen retain order", lastSeen: "0.4.0", notes: notes, want: notes[:2]},
		{name: "missing last seen", lastSeen: "0.3.9", notes: notes},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := Unseen(test.lastSeen, test.notes)
			if test.lastSeen == "" && got != nil {
				t.Fatalf("startup unseen = %#v, want nil", got)
			}
			if len(got) != len(test.want) {
				t.Fatalf("unseen = %#v, want %#v", got, test.want)
			}
			for i := range test.want {
				if got[i] != test.want[i] {
					t.Fatalf("unseen[%d] = %#v, want %#v", i, got[i], test.want[i])
				}
			}
		})
	}
}

func TestWatchLogsStartupUpdateAndFailureStreakOnce(t *testing.T) {
	initial := []gameapi.ReleaseNote{{Version: "0.4.0", ReleasedOn: "2026-10-26", Markdown: "# R&D"}}
	updated := []gameapi.ReleaseNote{
		{Version: "0.4.1", ReleasedOn: "2026-10-27", Markdown: "# Improvements"},
		initial[0],
	}
	sequence := []struct {
		notes []gameapi.ReleaseNote
		err   error
	}{
		{notes: initial},
		{notes: updated},
		{err: errors.New("release service unavailable")},
		{err: errors.New("release service still unavailable")},
		{notes: updated},
		{err: errors.New("second outage")},
		{err: errors.New("second outage continues")},
		{notes: updated},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	core, logs := observer.New(zap.DebugLevel)
	calls := 0
	fetch := func(context.Context) ([]gameapi.ReleaseNote, error) {
		if calls >= len(sequence) {
			cancel()
			return nil, ctx.Err()
		}
		result := sequence[calls]
		calls++
		return result.notes, result.err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		Watch(ctx, fetch, &Version{}, zap.New(core), time.Millisecond)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not process the fetch sequence and stop on cancellation")
	}
	if calls != len(sequence) {
		t.Fatalf("fetch calls = %d, want %d", calls, len(sequence))
	}
	startup := logs.FilterMessage("game release notes").All()
	if len(startup) != 1 {
		t.Fatalf("startup logs = %+v, want exactly one", startup)
	}
	if startup[0].Level != zap.InfoLevel || startup[0].ContextMap()["version"] != "0.4.0" {
		t.Fatalf("startup log = %+v, want INFO with version 0.4.0", startup[0])
	}
	updates := logs.FilterMessage("game updated while the bot is running").All()
	if len(updates) != 1 {
		t.Fatalf("update logs = %+v, want exactly one", updates)
	}
	if updates[0].Level != zap.WarnLevel || updates[0].ContextMap()["version"] != "0.4.1" || updates[0].ContextMap()["released_on"] != "2026-10-27" {
		t.Fatalf("update log = %+v, want WARN with version 0.4.1 and released_on", updates[0])
	}
	unavailable := logs.FilterMessage("game release notes unavailable").All()
	if len(unavailable) != 2 || unavailable[0].Level != zap.WarnLevel || unavailable[1].Level != zap.WarnLevel {
		t.Fatalf("unavailable logs = %+v, want one WARN per two-error streak", unavailable)
	}
	if logs.Len() != 4 {
		t.Fatalf("logs = %+v, want only startup, update, and two unavailable streaks", logs.All())
	}
}

func TestWatchEmptyBaselineAndMissingCursor(t *testing.T) {
	initial := gameapi.ReleaseNote{Version: "0.4.0"}
	updated := gameapi.ReleaseNote{Version: "0.4.1"}
	for _, test := range []struct {
		name        string
		feeds       [][]gameapi.ReleaseNote
		wantInfo    int
		wantUpdates int
	}{
		{"empty baseline", [][]gameapi.ReleaseNote{nil, {initial}, {updated, initial}, {updated, initial}}, 0, 2},
		{"missing cursor never moves backward", [][]gameapi.ReleaseNote{{updated, initial}, {initial}, {updated, initial}}, 1, 0},
		{"restart only logs info", [][]gameapi.ReleaseNote{{updated, initial}, {updated, initial}}, 1, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			core, logs := observer.New(zap.DebugLevel)
			index := 0
			Watch(ctx, func(context.Context) ([]gameapi.ReleaseNote, error) {
				if index == len(test.feeds) {
					cancel()
					return nil, ctx.Err()
				}
				notes := test.feeds[index]
				index++
				return notes, nil
			}, &Version{}, zap.New(core), time.Millisecond)
			if got := logs.FilterMessage("game release notes").Len(); got != test.wantInfo {
				t.Fatalf("startup logs = %d, want %d", got, test.wantInfo)
			}
			if got := logs.FilterMessage("game updated while the bot is running").Len(); got != test.wantUpdates {
				t.Fatalf("update logs = %d, want %d", got, test.wantUpdates)
			}
		})
	}
}

func TestReleaseLogBoundsAndFencedHeadings(t *testing.T) {
	markdown := "# Public\n```go\n# Not a heading\n```\n~~~\n# Also code\n~~~\n## " + strings.Repeat("界", 300)
	for range 10 {
		markdown += "\n### More"
	}
	headings := releaseHeadings(markdown)
	if len(headings) != 6 || headings[0] != "# Public" || utf8.RuneCountInString(headings[1]) != 200 {
		t.Fatalf("bounded headings = %#v", headings)
	}
	if strings.Contains(strings.Join(headings, "\n"), "code") || strings.Contains(strings.Join(headings, "\n"), "Not a heading") {
		t.Fatalf("fenced content entered headings: %#v", headings)
	}
	if got := boundedText("tw_live_secret", 64); got != "[REDACTED]" {
		t.Fatalf("public token was not redacted: %q", got)
	}
	initial := gameapi.ReleaseNote{Version: "0.4.0"}
	var updated []gameapi.ReleaseNote
	for index := range 20 {
		updated = append(updated, gameapi.ReleaseNote{
			Version:    fmt.Sprintf("%d%s", index, strings.Repeat("界", 100)),
			ReleasedOn: strings.Repeat("界", 100), Markdown: markdown,
		})
	}
	updated = append(updated, initial)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	core, logs := observer.New(zap.DebugLevel)
	calls := 0
	Watch(ctx, func(context.Context) ([]gameapi.ReleaseNote, error) {
		calls++
		switch calls {
		case 1:
			return []gameapi.ReleaseNote{initial}, nil
		case 2, 3:
			return updated, nil
		default:
			cancel()
			return nil, ctx.Err()
		}
	}, &Version{}, zap.New(core), time.Millisecond)
	updates := logs.FilterMessage("game updated while the bot is running").All()
	if len(updates) != 5 {
		t.Fatalf("update count = %d, want 5 bounded announcements without repeats", len(updates))
	}
	for _, entry := range updates {
		for _, field := range []string{"version", "released_on"} {
			if utf8.RuneCountInString(entry.ContextMap()[field].(string)) > 64 {
				t.Fatalf("unbounded %s", field)
			}
		}
		if got := entry.ContextMap()["headings"].([]interface{}); len(got) != 6 {
			t.Fatalf("logged headings = %#v", got)
		}
	}
}

func TestWatchPublishesNewestVersionAndRetainsItDuringOutages(t *testing.T) {
	sequence := []struct {
		notes []gameapi.ReleaseNote
		err   error
		want  string
	}{
		{},
		{err: errors.New("not yet available")},
		{notes: []gameapi.ReleaseNote{{Version: "0.4.0"}}, want: "0.4.0"},
		{notes: []gameapi.ReleaseNote{{Version: "0.5.0"}, {Version: "0.4.0"}}, want: "0.5.0"},
		{err: errors.New("temporary outage"), want: "0.5.0"},
		{want: "0.5.0"},
		// Even when the announcement cursor is absent, the newest-first
		// published feed still supplies the current route-cache version.
		{notes: []gameapi.ReleaseNote{{Version: "0.5.1"}}, want: "0.5.1"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var version Version
	index := 0
	Watch(ctx, func(context.Context) ([]gameapi.ReleaseNote, error) {
		want := ""
		if index > 0 {
			want = sequence[index-1].want
		}
		if got := version.Current(); got != want {
			t.Fatalf("published version after poll %d = %q, want %q", index, got, want)
		}
		if index == len(sequence) {
			cancel()
			return nil, ctx.Err()
		}
		result := sequence[index]
		index++
		return result.notes, result.err
	}, &version, zap.NewNop(), time.Millisecond)
	if index != len(sequence) {
		t.Fatalf("watcher stopped before publishing all version transitions: %d", index)
	}
}
