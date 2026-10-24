package gameapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.uber.org/zap"
)

var credentialPattern = regexp.MustCompile(`tw_(?:live|test|sk)_[A-Za-z0-9_-]+`)

func (c *Client) observe(method, path string, status int, duration time.Duration, request, response []byte, err error) {
	c.mu.RLock()
	fn := c.observer
	c.mu.RUnlock()

	if fn == nil && !c.log.Core().Enabled(zap.DebugLevel) {
		return
	}

	secrets := make(map[string]struct{})
	if c.apiKey != "" {
		secrets[c.apiKey] = struct{}{}
	}
	requestValue, requestValid := diagnosticJSON(request)
	responseValue, responseValid := diagnosticJSON(response)
	collectSecrets(requestValue, secrets)
	collectSecrets(responseValue, secrets)
	secretValues := make([]string, 0, len(secrets))
	for secret := range secrets {
		secretValues = append(secretValues, secret)
	}
	sort.Slice(secretValues, func(i, j int) bool {
		return len(secretValues[i]) > len(secretValues[j])
	})
	replacements := make([]string, 0, 2*len(secretValues))
	for _, secret := range secretValues {
		replacements = append(replacements, secret, "[REDACTED]")
	}
	replacer := strings.NewReplacer(replacements...)

	var safeRequest, safeResponse []byte
	if fn != nil {
		safeRequest = diagnosticPayload(request, requestValue, requestValid, replacer)
		safeResponse = diagnosticPayload(response, responseValue, responseValid, replacer)
	}
	safePath := redactPath(path, replacer)
	errText := ""
	if err != nil {
		errText = redactText(err.Error(), replacer)
	}
	c.log.Debug("game API request",
		zap.String("method", method),
		zap.String("path", safePath),
		zap.Int("status", status),
		zap.Duration("duration", duration),
		zap.String("error", errText),
	)
	if fn != nil {
		// Only application payloads are observable. Headers, the API key and
		// idempotency keys are deliberately never part of this interface.
		fn(method, safePath, status, duration, safeRequest, safeResponse, errText)
	}
}

func diagnosticJSON(body []byte) (any, bool) {
	if len(body) == 0 {
		return nil, true
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, false
	}
	return value, true
}

func collectSecrets(value any, secrets map[string]struct{}) {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if sensitiveName(key) {
				if secret, ok := child.(string); ok && secret != "" {
					secrets[secret] = struct{}{}
				}
			}
			collectSecrets(child, secrets)
		}
	case []any:
		for _, child := range value {
			collectSecrets(child, secrets)
		}
	}
}

func diagnosticPayload(original []byte, value any, valid bool, replacer *strings.Replacer) []byte {
	if len(original) == 0 {
		return nil
	}
	if !valid {
		// Truncated JSON or plain text cannot be safely redacted by field.
		return []byte(`"[unparseable payload redacted]"`)
	}
	redacted := redactJSON(value, replacer)
	body, err := json.Marshal(redacted)
	if err != nil {
		return []byte(`"[unrepresentable payload redacted]"`)
	}
	return body
}

func redactJSON(value any, replacer *strings.Replacer) any {
	switch value := value.(type) {
	case map[string]any:
		var renamed map[string]string
		var nextSuffix map[string]int
		for key, child := range value {
			if sensitiveName(key) {
				value[key] = "[REDACTED]"
			} else {
				value[key] = redactJSON(child, replacer)
			}
			safeKey := redactText(key, replacer)
			if safeKey != key {
				if renamed == nil {
					renamed = make(map[string]string)
					nextSuffix = make(map[string]int)
				}
				renamed[uniqueRedactedKey(safeKey, value, renamed, nextSuffix)] = key
			}
		}
		// Keep original names reserved until every destination is assigned.
		for safeKey, key := range renamed {
			value[safeKey] = value[key]
			delete(value, key)
		}
	case []any:
		for index, child := range value {
			value[index] = redactJSON(child, replacer)
		}
	case string:
		return redactText(value, replacer)
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

func sensitiveName(name string) bool {
	name = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, name)
	for _, marker := range []string{"passphrase", "password", "apikey", "authorization", "credential", "secret", "token", "masterkey", "privatekey", "cookie"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

func redactText(text string, replacer *strings.Replacer) string {
	return credentialPattern.ReplaceAllString(replacer.Replace(text), "[REDACTED]")
}

func redactPath(path string, replacer *strings.Replacer) string {
	parsed, err := url.ParseRequestURI(path)
	if err != nil {
		return "[invalid path]"
	}
	if parsed.RawQuery != "" {
		query, err := url.ParseQuery(parsed.RawQuery)
		if err != nil {
			parsed.RawQuery = "[invalid query redacted]"
		} else {
			var renamed map[string]string
			var nextSuffix map[string]int
			for key, values := range query {
				sensitive := sensitiveName(key)
				for index, value := range values {
					if sensitive {
						values[index] = "[REDACTED]"
					} else {
						values[index] = redactText(value, replacer)
					}
				}
				safeKey := redactText(key, replacer)
				if safeKey != key {
					if renamed == nil {
						renamed = make(map[string]string)
						nextSuffix = make(map[string]int)
					}
					renamed[uniqueRedactedKey(safeKey, query, renamed, nextSuffix)] = key
				}
			}
			for safeKey, key := range renamed {
				query[safeKey] = query[key]
				delete(query, key)
			}
			parsed.RawQuery = query.Encode()
		}
	}
	parsed.Path = redactText(parsed.Path, replacer)
	parsed.RawPath = ""
	return parsed.String()
}
