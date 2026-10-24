package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactJSONRecursiveSecretsAndIntegerPrecision(t *testing.T) {
	raw := []byte(`{"quantity":9007199254740993,"passphrase":"private phrase","nested":[{"API-Key":"unknown key","detail":"known-key appears here"}],"headers":{"X-Test":"never persist headers"}}`)
	clean, err := redactJSON(raw, []string{"private phrase", "known-key"}, maxDiagnosticBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(clean) || !strings.Contains(string(clean), "9007199254740993") {
		t.Fatalf("redaction changed JSON validity or integer precision: %s", clean)
	}
	for _, secret := range []string{"private phrase", "known-key", "unknown key", "never persist headers"} {
		if strings.Contains(string(clean), secret) {
			t.Fatalf("recursive secret survived: %q", secret)
		}
	}
}

func TestRedactJSONMemberNamesAndValues(t *testing.T) {
	raw := []byte(`{"public":{"fleet_count":2},"known phrase":{"detail":"known-key"},"nested":[{"tw_test_unlisted_member":"visible","Bearer member-token":"visible","password=assigned-member-secret":"visible"}]}`)
	clean, err := redactJSON(raw, []string{"known phrase", "known-key"}, maxDiagnosticBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"known phrase", "known-key", "tw_test_unlisted_member", "member-token", "assigned-member-secret"} {
		if strings.Contains(string(clean), secret) {
			t.Fatalf("credential survived in a JSON name or value: %q", secret)
		}
	}
	var decoded struct {
		Public struct {
			FleetCount int `json:"fleet_count"`
		} `json:"public"`
	}
	if err := json.Unmarshal(clean, &decoded); err != nil || decoded.Public.FleetCount != 2 {
		t.Fatalf("ordinary public data changed during member-name redaction: %s, %v", clean, err)
	}
}

func TestRedactJSONMemberNameCollisionsPreserveValues(t *testing.T) {
	members := `{"[REDACTED]":"existing","[REDACTED]_2":"existing suffix","tw_test_member_one":"first","tw_test_member_two":"second","known-collision":"third"}`
	raw := []byte(`{"root":` + members + `,"nested":[` + members + `]}`)
	clean, err := redactJSON(raw, []string{"known-collision"}, maxDiagnosticBytes)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Root   map[string]string   `json:"root"`
		Nested []map[string]string `json:"nested"`
	}
	if err := json.Unmarshal(clean, &decoded); err != nil || len(decoded.Nested) != 1 {
		t.Fatalf("collision-safe redaction changed the data structure: %s, %v", clean, err)
	}
	for _, group := range []map[string]string{decoded.Root, decoded.Nested[0]} {
		if len(group) != 5 || group[redacted] != "existing" || group[redacted+"_2"] != "existing suffix" {
			t.Fatalf("renamed members overwrote existing public names: %+v", group)
		}
		seen := make(map[string]bool)
		for key, value := range group {
			if apiKeyPattern.MatchString(key) || strings.Contains(key, "known-collision") {
				t.Fatalf("colliding credential name survived: %q", key)
			}
			seen[value] = true
		}
		for _, value := range []string{"existing", "existing suffix", "first", "second", "third"} {
			if !seen[value] {
				t.Fatalf("colliding member value was discarded or changed: %q in %+v", value, group)
			}
		}
	}
}

func TestRedactOpaqueBodiesAndEncodedQuery(t *testing.T) {
	clean, err := redactJSON([]byte("upstream failed using Bearer opaque-token and tw_live_unexpectedkey"), nil, maxDiagnosticBytes)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(clean), "opaque-token") || strings.Contains(string(clean), "tw_live_unexpectedkey") {
		t.Fatalf("opaque response leaked credentials: %s", clean)
	}
	path := redactPath("/v1/example?api_key=encoded%20secret&safe=own%20passphrase&limit=20", []string{"own passphrase"})
	if strings.Contains(path, "encoded") || strings.Contains(path, "passphrase") || !strings.Contains(path, "limit=20") {
		t.Fatalf("query redaction lost public fields or exposed a secret: %s", path)
	}
}

func TestRedactMalformedJSONWithoutPersistingUnredactableFields(t *testing.T) {
	clean, err := redactJSON([]byte(`{"headers":{"Authorization":"unknown header"},"password":"unknown phrase with spaces`), nil, maxDiagnosticBytes)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(clean), "unknown") || !strings.Contains(string(clean), "unparsed") {
		t.Fatalf("malformed structured content was not explicitly omitted: %s", clean)
	}
}

func TestRedactOversizedDiagnosticIsExplicitAndBounded(t *testing.T) {
	clean, err := redactJSON([]byte(`{"detail":"`+strings.Repeat("x", maxDiagnosticBytes*2)+`","api_key":"hidden"}`), nil, maxDiagnosticBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(clean) > maxDiagnosticBytes || !json.Valid(clean) || !strings.Contains(string(clean), `"truncated":true`) || strings.Contains(string(clean), "hidden") {
		t.Fatalf("diagnostic cap or redaction failed: bytes=%d", len(clean))
	}
}

func TestRedactSupportedTokenPrefixesAcrossDiagnosticSurfaces(t *testing.T) {
	for _, token := range []string{
		"tw_live_other_account_Ab9-xy",
		"tw_test_expired_Ab9-xy",
		"tw_sk_other_account_Ab9-xy",
		"tw_sk_test_expired_Ab9-xy",
	} {
		t.Run(token, func(t *testing.T) {
			raw := []byte(`{"` + token + `":"public value","detail":"echo ` + token + `","count":7}`)
			body, err := redactJSON(raw, nil, maxDiagnosticBytes)
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range []string{
				string(body),
				scrubText("upstream echoed "+token, nil),
				redactPath("/v1/example?detail="+token+"&"+token+"=public&limit=7", nil),
			} {
				if strings.Contains(value, token) {
					t.Fatalf("supported credential prefix survived diagnostic scrubbing: %s", value)
				}
			}
			var decoded map[string]any
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded[redacted] != "public value" || decoded["count"] != float64(7) {
				t.Fatalf("scrubbing discarded public diagnostic data: %s", body)
			}
		})
	}
}
