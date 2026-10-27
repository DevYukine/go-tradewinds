package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBoundEventPayloadKeepsOrdinaryNumbersAndGapsExpansions(t *testing.T) {
	nested := func(depth int) string {
		return strings.Repeat("[", depth) + strings.Repeat("]", depth)
	}
	cases := []struct {
		name    string
		payload string
		gapped  bool
	}{
		{"plain integer", `{"pence":123456789}`, false},
		{"small exponent float", `{"tons":2.5e-7,"speed":1E3}`, false},
		{"exponent at bound", `[1e32,1e-32]`, false},
		{"zero with huge scale", `[0e-16000]`, true},
		{"huge exponent", `[1e130000]`, true},
		{"exponent just past bound", `[1e33]`, true},
		{"32 character number", `[` + strings.Repeat("1", 32) + `]`, false},
		{"33 character number", `[` + strings.Repeat("1", 33) + `]`, true},
		{"depth 64", nested(64), false},
		{"depth 65", nested(65), true},
		{"malformed passes through", `{"a":`, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			bounded := BoundEventPayload(json.RawMessage(test.payload))
			gapped := strings.Contains(string(bounded), `"gap":true`)
			if gapped != test.gapped {
				t.Fatalf("gapped = %v, want %v (payload %q)", gapped, test.gapped, bounded)
			}
			if !gapped && string(bounded) != test.payload {
				t.Fatalf("kept payload changed: %q", bounded)
			}
		})
	}
}
