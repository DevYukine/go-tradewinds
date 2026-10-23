package gameapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const maxEventBytes = 1 << 20

// Stream opens exactly one connection. It has no ordinary request/body timeout;
// the caller cancels it and owns reconnect, cursor persistence and REST resync.
func (c *Client) Stream(ctx context.Context, afterID string, onEvent func(Event) error) error {
	if c.initErr != nil {
		return c.initErr
	}
	if onEvent == nil {
		return errors.New("game API stream requires an event callback")
	}
	if !validHeaderValue(afterID) {
		return errors.New("game API stream cursor contains invalid header characters")
	}
	const path = "/v1/event/sse"
	requestURL, err := c.requestURL(path)
	if err != nil {
		return err
	}
	if err := c.rates.wait(ctx, streamBucket, c.keyHash, c.apiKey != ""); err != nil {
		return err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(streamCtx, http.MethodGet, requestURL, nil)
	if err != nil {
		return &requestFailure{message: "cannot create game API stream request", cause: err}
	}
	c.setHeaders(request, "text/event-stream")
	request.Header.Set("Cache-Control", "no-cache")
	if afterID != "" {
		request.Header.Set("Last-Event-ID", afterID)
	}

	start := time.Now()
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		} else {
			err = &requestFailure{message: "game API stream transport failed", cause: err}
		}
		c.observe(http.MethodGet, path, 0, time.Since(start), nil, nil, err)
		return err
	}
	cooldown := c.rates.update(streamBucket, c.keyHash, c.apiKey != "", response)

	contentType, _, contentErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusOK || contentErr != nil || !strings.EqualFold(contentType, "text/event-stream") {
		// Bound a rejected handshake's body without imposing a lifetime on a
		// successful event connection.
		timer := time.AfterFunc(requestTimeout, cancel)
		body, readErr := readBody(response)
		timer.Stop()
		failure := apiError(response.StatusCode, body, readErr)
		if response.StatusCode == http.StatusOK && readErr == nil {
			failure.Code = "invalid_stream_response"
			failure.Detail = "game API stream did not return text/event-stream"
		}
		failure.Replayed = strings.EqualFold(response.Header.Get("Idempotency-Replayed"), "true")
		failure.RetryAfter = cooldown
		c.observe(http.MethodGet, path, response.StatusCode, time.Since(start), nil, body, failure)
		if response.StatusCode == http.StatusTooManyRequests {
			if err := c.rates.waitPause(ctx, streamBucket, c.keyHash, c.apiKey != ""); err != nil {
				failure.cause = errors.Join(failure.cause, err)
			}
		}
		return failure
	}
	defer response.Body.Close()

	err = parseEvents(ctx, response.Body, afterID, onEvent)
	c.observe(http.MethodGet, path, response.StatusCode, time.Since(start), nil, nil, err)
	return err
}

func parseEvents(ctx context.Context, source io.Reader, afterID string, onEvent func(Event) error) error {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 4096), maxEventBytes+1)
	scanner.Split(splitEventLines)

	currentID := afterID
	eventType := ""
	var data []byte
	hasData := false
	hasFields := false
	frameBytes := 0
	firstLine := true

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Bytes()
		if firstLine {
			line = bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
			firstLine = false
		}
		frameBytes += len(line) + 1
		if frameBytes > maxEventBytes {
			return errors.New("game API SSE frame exceeds 1 MiB")
		}

		if len(line) == 0 {
			if hasData {
				payload := data[:len(data)-1]
				if !utf8.Valid(payload) || !json.Valid(payload) {
					return errors.New("game API SSE data is malformed JSON")
				}
				if eventType == "" {
					eventType = "message"
				}
				event := Event{ID: currentID, Type: eventType, Data: payload}
				if err := onEvent(event); err != nil {
					// The caller must not advance its durable cursor when its
					// transaction or downstream processing has failed.
					return err
				}
				switch event.Type {
				case "gap":
					return &APIError{
						Status: http.StatusGone,
						Code:   "events_expired",
						Detail: "SSE cursor gap requires REST resynchronization",
						Body:   event.Data,
					}
				case "auth_revoked", "auth_expired":
					return &APIError{
						Status: http.StatusUnauthorized,
						Code:   event.Type,
						Detail: "SSE authentication ended",
						Body:   event.Data,
					}
				}
			}
			eventType = ""
			data = nil
			hasData = false
			hasFields = false
			frameBytes = 0
			continue
		}
		if line[0] == ':' {
			continue
		}

		field, value, _ := bytes.Cut(line, []byte(":"))
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		switch string(field) {
		case "id":
			if !bytes.ContainsRune(value, '\x00') {
				if !utf8.Valid(value) {
					return errors.New("game API SSE id is not valid UTF-8")
				}
				currentID = string(value)
			}
			hasFields = true
		case "event":
			if !utf8.Valid(value) {
				return errors.New("game API SSE event type is not valid UTF-8")
			}
			eventType = string(value)
			hasFields = true
		case "data":
			data = append(data, value...)
			data = append(data, '\n')
			hasData = true
			hasFields = true
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return &requestFailure{message: "cannot read game API SSE line (1 MiB limit)", cause: err}
		}
		return &requestFailure{message: "game API stream transport failed", cause: err}
	}
	if hasData || hasFields {
		return &requestFailure{message: "game API SSE ended with an incomplete frame", cause: io.ErrUnexpectedEOF}
	}
	return nil
}

// SSE recognizes CR, LF and CRLF, including CRLF split across network reads.
func splitEventLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for index, char := range data {
		switch char {
		case '\n':
			return index + 1, data[:index], nil
		case '\r':
			if index+1 == len(data) && !atEOF {
				return 0, nil, nil
			}
			advance = index + 1
			if index+1 < len(data) && data[index+1] == '\n' {
				advance++
			}
			return advance, data[:index], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
