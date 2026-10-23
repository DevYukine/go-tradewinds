package gameapi

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// acceptTransportReset stands in for any network-level read failure: connection
// resets, TLS truncation, or a dropped socket observed mid-stream.
var acceptTransportReset = errors.New("tcp connection reset mid-stream")

// acceptFailingReader always fails with the same error, deterministically.
type acceptFailingReader struct {
	err error
}

func (r acceptFailingReader) Read([]byte) (int, error) {
	return 0, r.err
}

// acceptAssertTransportFailure pins the classification contract for reader
// failures: they name the transport, never the 1 MiB scanner limit, and keep
// the original cause reachable for callers deciding whether to reconnect.
func acceptAssertTransportFailure(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("transport reset returned nil error")
	}
	if errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("transport reset misclassified as scanner size limit: %v", err)
	}
	if !errors.Is(err, acceptTransportReset) {
		t.Fatalf("transport cause was not preserved through the failure: %v", err)
	}
	message := strings.ToLower(err.Error())
	if !strings.Contains(message, "transport") {
		t.Fatalf("transport failure diagnostic does not name the transport: %q", err.Error())
	}
	if strings.Contains(message, "mib") {
		t.Fatalf("transport failure diagnostic still claims the 1 MiB limit: %q", err.Error())
	}
}

// acceptAssertSizeLimitFailure pins the classification contract for frames
// that genuinely exceed maxEventBytes: the diagnostic states the 1 MiB limit
// and the scanner cause stays preserved.
func acceptAssertSizeLimitFailure(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("oversized frame returned nil error")
	}
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("size-limit cause was not preserved: %v", err)
	}
	message := err.Error()
	if !strings.Contains(message, "1 MiB") {
		t.Fatalf("oversized frame diagnostic does not state the 1 MiB limit: %q", message)
	}
	if strings.Contains(strings.ToLower(message), "transport") {
		t.Fatalf("oversized frame was misclassified as a transport failure: %q", message)
	}
}

// A transport read failure is a reconnectable stream error, not evidence that
// any frame exceeded the 1 MiB scanner limit. Delivered frames stay delivered.
func TestAcceptSSETransportResetReportsTransportFailureNotSizeLimit(t *testing.T) {
	var events []Event
	err := parseEvents(context.Background(), acceptFailingReader{err: acceptTransportReset}, "0", func(event Event) error {
		events = append(events, event)
		return nil
	})
	acceptAssertTransportFailure(t, err)
	if len(events) != 0 {
		t.Fatalf("failing transport delivered %d events", len(events))
	}

	var midStream []Event
	stream := io.MultiReader(
		strings.NewReader("id: 7\ndata: {\"ok\":true}\n\n"),
		acceptFailingReader{err: acceptTransportReset},
	)
	err = parseEvents(context.Background(), stream, "0", func(event Event) error {
		midStream = append(midStream, event)
		return nil
	})
	acceptAssertTransportFailure(t, err)
	if len(midStream) != 1 || string(midStream[0].Data) != `{"ok":true}` || midStream[0].ID != "7" {
		t.Fatalf("complete frame before the reset was lost: %+v", midStream)
	}
}

// One line longer than the scanner buffer is a genuine size-limit failure.
func TestAcceptSSEOversizedSingleLineStillReportsSizeLimit(t *testing.T) {
	line := "data: " + strings.Repeat("a", maxEventBytes) + "\n\n"
	var events []Event
	err := parseEvents(context.Background(), strings.NewReader(line), "0", func(event Event) error {
		events = append(events, event)
		return nil
	})
	acceptAssertSizeLimitFailure(t, err)
	if len(events) != 0 {
		t.Fatalf("oversized line delivered %d events", len(events))
	}
}

// Many short lines that together exceed maxEventBytes fail on the frame
// accounting, not on the scanner buffer, so the cause is not ErrTooLong.
func TestAcceptSSEOversizedMultilineFrameStillReportsSizeLimit(t *testing.T) {
	line := "data: " + strings.Repeat("x", 1<<16)
	input := strings.Repeat(line+"\n", 16)
	var events []Event
	err := parseEvents(context.Background(), strings.NewReader(input), "0", func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err == nil {
		t.Fatal("oversized multiline frame returned nil error")
	}
	if errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("multiline frame was misclassified as a single-line scanner failure: %v", err)
	}
	if !strings.Contains(err.Error(), "1 MiB") {
		t.Fatalf("multiline frame diagnostic does not state the 1 MiB limit: %q", err.Error())
	}
	if len(events) != 0 {
		t.Fatalf("oversized frame delivered %d events", len(events))
	}
}

// The 1 MiB frame accounting is inclusive: a frame of exactly maxEventBytes is
// delivered, and one byte more fails.
func TestAcceptSSEFrameLimitBoundaryIsInclusive(t *testing.T) {
	payload := `"` + strings.Repeat("a", maxEventBytes-10) + `"`
	exact := "data: " + payload + "\n\n"
	// strings.Reader mirrors realistic network chunks; byte-at-a-time reads on
	// a megabyte-scale frame would make the existing scanner work quadratic.
	var events []Event
	err := parseEvents(context.Background(), strings.NewReader(exact), "cursor-9", func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("frame of exactly maxEventBytes bytes was rejected: %v", err)
	}
	if len(events) != 1 || events[0].ID != "cursor-9" || events[0].Type != "message" || string(events[0].Data) != payload {
		t.Fatalf("boundary frame was not delivered intact: %+v", events)
	}

	oversizedPayload := `"` + strings.Repeat("a", maxEventBytes-9) + `"`
	var rejected []Event
	err = parseEvents(context.Background(), strings.NewReader("data: "+oversizedPayload+"\n\n"), "0", func(event Event) error {
		rejected = append(rejected, event)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Fatalf("frame one byte over the limit did not fail with the size diagnostic: %v", err)
	}
	if len(rejected) != 0 {
		t.Fatalf("frame one byte over the limit delivered %d events", len(rejected))
	}
}

// A frame truncated by EOF stays an error and never reaches the callback,
// while CR/CRLF split across individual reads still assembles one frame.
func TestAcceptSSEIncompleteFrameAndSplitCRLFBehaveAsBefore(t *testing.T) {
	var truncated []Event
	err := parseEvents(context.Background(), strings.NewReader("event: ping\ndata: {\"n\":1}"), "1", func(event Event) error {
		truncated = append(truncated, event)
		return nil
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated frame did not keep the unexpected-EOF cause: %v", err)
	}
	if len(truncated) != 0 {
		t.Fatalf("incomplete frame invoked the event callback: %+v", truncated)
	}

	// Two data lines carrying one valid JSON payload, delivered one byte at a
	// time so CR and LF reach the splitter in separate reads.
	var events []Event
	input := "data: {\"n\":\r\ndata: 1}\r\n\r\n"
	err = parseEvents(context.Background(), iotest.OneByteReader(strings.NewReader(input)), "cursor-9", func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("split CRLF frame failed: %v", err)
	}
	if len(events) != 1 || events[0].ID != "cursor-9" || events[0].Type != "message" || string(events[0].Data) != "{\"n\":\n1}" {
		t.Fatalf("split CRLF frame was not assembled intact: %+v", events)
	}
}

// Cancellation outranks both the transport classification and a clean end of
// stream, and a canceled stream never delivers the pending event.
func TestAcceptSSECancellationPrecedesTransportAndCleanEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var events []Event
	stream := io.MultiReader(
		strings.NewReader("id: 1\ndata: {\"early\":true}\n\n"),
		acceptFailingReader{err: acceptTransportReset},
	)
	err := parseEvents(ctx, stream, "0", func(event Event) error {
		events = append(events, event)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled context did not return its own error: %v", err)
	}
	if errors.Is(err, acceptTransportReset) {
		t.Fatalf("cancellation lost to the transport classification: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("canceled stream delivered events: %+v", events)
	}

	callbackCtx, callbackCancel := context.WithCancel(context.Background())
	var callbackCalls int
	err = parseEvents(callbackCtx, strings.NewReader("data: {\"a\":1}\n\n"), "0", func(event Event) error {
		callbackCalls++
		callbackCancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("callback-initiated cancellation was swallowed by clean EOF: %v", err)
	}
	if callbackCalls != 1 {
		t.Fatalf("cancellation callback ran %d times", callbackCalls)
	}
}

// A callback error is the caller's own failure and must come back unchanged.
func TestAcceptSSECallbackErrorStaysUnwrappedAndUnchanged(t *testing.T) {
	callbackFailure := errors.New("cursor transaction rolled back")
	var calls int
	err := parseEvents(context.Background(), strings.NewReader("data: {\"a\":1}\n\ndata: {\"b\":2}\n\n"), "0", func(event Event) error {
		calls++
		return callbackFailure
	})
	if !errors.Is(err, callbackFailure) {
		t.Fatalf("callback error was replaced or hidden: %v", err)
	}
	if calls != 1 {
		t.Fatalf("callback ran %d times after returning an error", calls)
	}
	if strings.Contains(strings.ToLower(err.Error()), "transport") {
		t.Fatalf("callback failure was misclassified as a transport failure: %q", err.Error())
	}
}
