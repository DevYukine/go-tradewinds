package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const redacted = "[REDACTED]"

var (
	bearerPattern     = regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/=-]+`)
	apiKeyPattern     = regexp.MustCompile(`tw_(?:live|test|sk)_[A-Za-z0-9_-]+`)
	assignmentPattern = regexp.MustCompile(`(?i)\b[a-z0-9_-]*(?:api[_-]?key|pass(?:word|phrase)|authorization|secret|credential|token|cookie)[a-z0-9_-]*["']?\s*[:=]\s*(?:"(?:[^"\\]|\\.)*(?:"|$)|'(?:[^'\\]|\\.)*(?:'|$)|[^\s&,;]+)`)
)

func unparsedDiagnostic(raw []byte, secrets []string) any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) != 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		return map[string]any{
			"unparsed":       true,
			"original_bytes": len(raw),
			"omitted":        "invalid_json_cannot_be_safely_redacted",
		}
	}
	return scrubText(string(raw), secrets)
}

func (s *Store) secrets(ctx context.Context, accountID string) ([]string, error) {
	credentials, err := s.Credentials(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return credentialSecrets(credentials), nil
}

func credentialSecrets(credentials Credentials) []string {
	values := make([]string, 0, 2)
	if credentials.Passphrase != "" {
		values = append(values, credentials.Passphrase)
	}
	if credentials.APIKey != "" {
		values = append(values, credentials.APIKey)
	}
	return values
}

// ScrubOutcomeTexts redacts a batch of upstream-derived outcome strings with
// the account's current credentials and the shared credential patterns. It
// fails closed: when the credentials cannot be read, every value becomes the
// redaction marker, so a caller logging the result can never emit an
// unsanitized upstream string.
func (s *Store) ScrubOutcomeTexts(ctx context.Context, accountID string, values []string) []string {
	secrets, err := s.secrets(ctx, accountID)
	if err != nil {
		censored := make([]string, len(values))
		for i := range values {
			censored[i] = redacted
		}
		return censored
	}
	cleaned := make([]string, len(values))
	for i, value := range values {
		cleaned[i] = scrubText(value, secrets)
	}
	return cleaned
}

func sensitiveKey(key string) bool {
	normalized := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, key)
	if normalized == "headers" || normalized == "key" || normalized == "cookie" || normalized == "setcookie" {
		return true
	}
	for _, part := range []string{"password", "passphrase", "apikey", "authorization", "secret", "credential", "accesstoken", "refreshtoken", "privatetoken", "privatekey"} {
		if strings.Contains(normalized, part) {
			return true
		}
	}
	return normalized == "token"
}

func scrubText(value string, secrets []string) string {
	for _, secret := range secrets {
		value = strings.ReplaceAll(value, secret, redacted)
	}
	// Each regexp requires these literal bytes. Native scans avoid running
	// the regexp machine over large opaque strings that cannot match.
	if strings.ContainsAny(value, " \t\n\f\r") {
		value = bearerPattern.ReplaceAllString(value, "Bearer "+redacted)
	}
	if strings.Contains(value, "tw_") {
		value = apiKeyPattern.ReplaceAllString(value, redacted)
	}
	if delimiter := strings.IndexAny(value, ":="); delimiter >= 0 && delimiter < len(value)-1 {
		value = assignmentPattern.ReplaceAllString(value, redacted)
	}
	return value
}

func redactValue(value any, secrets []string) any {
	switch typed := value.(type) {
	case map[string]any:
		var renamed map[string]string
		var nextSuffix map[string]int
		for key, child := range typed {
			if sensitiveKey(key) {
				typed[key] = redacted
			} else {
				typed[key] = redactValue(child, secrets)
			}
			cleanKey := scrubText(key, secrets)
			if cleanKey == key {
				continue
			}
			if renamed == nil {
				renamed = make(map[string]string)
				nextSuffix = make(map[string]int)
			}
			renamed[uniqueRedactedKey(cleanKey, typed, renamed, nextSuffix)] = key
		}
		// Keep original names reserved until every destination is assigned.
		for cleanKey, key := range renamed {
			typed[cleanKey] = typed[key]
			delete(typed, key)
		}
	case []any:
		for i, child := range typed {
			typed[i] = redactValue(child, secrets)
		}
	case string:
		return scrubText(typed, secrets)
	}
	return value
}

func uniqueRedactedKey[V any](base string, original map[string]V, renamed map[string]string, nextSuffix map[string]int) string {
	key := base
	suffix := nextSuffix[base]
	if suffix < 2 {
		suffix = 2
	}
	for {
		_, existing := original[key]
		_, assigned := renamed[key]
		if !existing && !assigned {
			nextSuffix[base] = suffix
			return key
		}
		key = base + "_" + strconv.Itoa(suffix)
		suffix++
	}
}

func redactJSON(raw []byte, secrets []string, maxBytes int) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("null"), nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		value = unparsedDiagnostic(raw, secrets)
	} else {
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			value = unparsedDiagnostic(raw, secrets)
		} else {
			value = redactValue(value, secrets)
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode redacted diagnostic: %w", err)
	}
	if maxBytes == 0 || len(encoded) <= maxBytes {
		return encoded, nil
	}
	// Truncation is explicit and happens only after redaction. Never retain an
	// unsanitized byte prefix or pretend an oversized upstream reply was empty.
	previewBytes := maxBytes / 4
	preview := boundedText(string(encoded), previewBytes)
	return json.Marshal(struct {
		Truncated     bool   `json:"truncated"`
		OriginalBytes int    `json:"original_bytes"`
		Preview       string `json:"preview"`
	}{Truncated: true, OriginalBytes: len(raw), Preview: preview})
}

func boundedText(value string, limit int) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func redactPath(path string, secrets []string) string {
	parsed, err := url.Parse(path)
	if err != nil {
		return boundedText(scrubText(path, secrets), 4096)
	}
	parsed.User = nil
	values := parsed.Query()
	var renamed map[string]string
	var nextSuffix map[string]int
	for key, entries := range values {
		sensitive := sensitiveKey(key)
		for i, entry := range entries {
			if sensitive {
				entries[i] = redacted
			} else {
				entries[i] = scrubText(entry, secrets)
			}
		}
		cleanKey := scrubText(key, secrets)
		if cleanKey != key {
			if renamed == nil {
				renamed = make(map[string]string)
				nextSuffix = make(map[string]int)
			}
			renamed[uniqueRedactedKey(cleanKey, values, renamed, nextSuffix)] = key
		}
	}
	for cleanKey, key := range renamed {
		values[cleanKey] = values[key]
		delete(values, key)
	}
	parsed.RawQuery = values.Encode()
	parsed.Scheme = scrubText(parsed.Scheme, secrets)
	parsed.Opaque = scrubText(parsed.Opaque, secrets)
	parsed.Host = scrubText(parsed.Host, secrets)
	parsed.Path = scrubText(parsed.Path, secrets)
	parsed.RawPath = ""
	parsed.Fragment = scrubText(parsed.Fragment, secrets)
	parsed.RawFragment = ""
	return boundedText(parsed.String(), 4096)
}
