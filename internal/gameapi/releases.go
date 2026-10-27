package gameapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
)

// ReleaseNote is an entry from the public, newest-first release feed.
type ReleaseNote struct {
	Version    string `json:"version"`
	ReleasedOn string `json:"released_on"`
	Markdown   string `json:"markdown"`
}

func (c *Client) ReleaseNotes(ctx context.Context) ([]ReleaseNote, error) {
	if c.initErr != nil {
		return nil, c.initErr
	}
	// Feed headers must never clamp authenticated accounts' shared IP buckets.
	// Keep this client across polls so bounded public cooldowns still apply.
	c.releaseOnce.Do(func() {
		c.releaseClient = New(c.base.String(), "", c.log)
		c.releaseClient.rates = &hostRates{
			ip:         newBuckets(5),
			publicFeed: true,
		}
	})
	var raw json.RawMessage
	if err := c.releaseClient.Get(ctx, "/update", &raw); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if token != json.Delim('[') {
		return nil, errors.New("release feed is not an array")
	}
	// The response is already syntactically validated by Get. Bound decoded
	// history as well as response bytes; a missing cursor is ignored by Watch.
	var notes []ReleaseNote
	for decoder.More() && len(notes) < 100 {
		var note ReleaseNote
		if err := decoder.Decode(&note); err != nil {
			return nil, err
		}
		notes = append(notes, note)
	}
	return notes, nil
}

// RedactPublicText removes credential-shaped tokens from unauthenticated logs.
func RedactPublicText(text string) string {
	return credentialPattern.ReplaceAllString(text, "[REDACTED]")
}
