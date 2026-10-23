package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"go.uber.org/zap"
)

const (
	// acceptHelperEnv turns the test binary into the logger subprocess;
	// acceptScenarioEnv selects which deterministic log script it runs.
	acceptHelperEnv   = "TRADEWINDS_LOGGER_ACCEPTANCE_HELPER"
	acceptScenarioEnv = "TRADEWINDS_LOGGER_ACCEPTANCE_SCENARIO"
	acceptHelperName  = "TestLoggerAcceptanceStderrHelper"

	// acceptCallerMutationExit is the helper's exit code when logging mutated
	// the caller's variadic field slice.
	acceptCallerMutationExit = 7
)

// acceptHostileMessage carries every control category the console output must
// contain: ANSI CSI, newline, carriage return, a raw C1 byte, invalid-ish
// bytes, and the Unicode NEL, line-separator, paragraph-separator, and
// bidi-override controls.
var acceptHostileMessage = "bad\x1b[31mred" +
	"\nNEWLINE" +
	"\rCR" +
	"\x9bRAW-C1" +
	"\u0085NEL" +
	"\u2028LS" +
	"\u2029PS" +
	"\u202eBIDI"

// acceptJSONNormalizedHostile is what any valid JSON remainder can carry of
// the hostile message: JSON strings cannot hold the raw invalid C1 byte, so
// the standard lossy normalization replaces it with U+FFFD. Every other
// control must survive losslessly.
var acceptJSONNormalizedHostile = strings.ReplaceAll(acceptHostileMessage, "\x9b", "\uFFFD")

// acceptANSI matches encoder-owned SGR sequences so colored and plain output
// can be compared for content equivalence.
var acceptANSI = regexp.MustCompile("\x1b\\[[0-9;]*m")

// TestLoggerAcceptanceStderrHelper is re-executed by the acceptance tests as a
// subprocess so the logger's real stderr can be captured through a pipe
// without touching production code or guessing at logger internals.
func TestLoggerAcceptanceStderrHelper(t *testing.T) {
	if os.Getenv(acceptHelperEnv) != "1" {
		t.Skip("logger stderr helper; runs only as a subprocess")
	}
	logger, err := newLogger()
	if err != nil {
		fmt.Fprintf(os.Stderr, "logger helper rejected the environment: %v\n", err)
		os.Exit(2)
	}
	switch os.Getenv(acceptScenarioEnv) {
	case "fields":
		account := logger.Named("executor").With(
			zap.String("account", "Yukine"),
			zap.String("account_id", "acc-7"),
			zap.String("action_id", "act-42"),
			zap.Int("cargo_tons", 120),
			zap.Bool("execute", true),
		)
		account.Info("dispatch decision")
		logger.Info("dashboard ready")
		logger.Warn("retention sweep failed")
	case "levels":
		logger.Debug("debug probe")
		logger.Info("info probe")
		logger.Warn("warn probe")
	case "clone":
		base := logger.With(
			zap.String("scope", "base"),
			zap.String("base_only", "b"),
		)
		derived := base.With(zap.String("derived_only", "d"))
		sibling := base.With(zap.String("sibling_only", "s"))
		empty := logger.With()
		base.Info("base message")
		derived.Info("derived message")
		sibling.Info("sibling message")
		empty.Info("empty message")
		callerFields := []zap.Field{zap.String("kept", "v")}
		snapshot := append([]zap.Field(nil), callerFields...)
		logger.With(callerFields...).Info("mutation probe")
		if !reflect.DeepEqual(callerFields, snapshot) {
			// The caller's variadic slice must survive logging untouched.
			os.Exit(acceptCallerMutationExit)
		}
	case "injection":
		logger.Info(acceptHostileMessage)
		logger.Named("sub\u001bsystem").With(
			zap.String("note", acceptHostileMessage),
			zap.String("k\u001bey", "val\u2028ue"),
		).Info(acceptHostileMessage)
	default:
		fmt.Fprintf(os.Stderr, "logger helper got unknown scenario %q\n", os.Getenv(acceptScenarioEnv))
		os.Exit(2)
	}
	_ = logger.Sync()
}

// acceptSubprocessEnv strips every logger-related variable from the inherited
// environment before applying the scenario overrides, so a developer shell
// cannot leak TERM, NO_COLOR, or LOG_* into the captured output.
func acceptSubprocessEnv(overrides map[string]string) []string {
	controlled := map[string]bool{
		"LOG_LEVEL":       true,
		"LOG_COLOR":       true,
		"NO_COLOR":        true,
		"TERM":            true,
		acceptHelperEnv:   true,
		acceptScenarioEnv: true,
	}
	env := make([]string, 0, len(os.Environ())+len(overrides)+2)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if controlled[strings.ToUpper(name)] {
			continue
		}
		env = append(env, entry)
	}
	for name, value := range overrides {
		env = append(env, name+"="+value)
	}
	return append(env, acceptHelperEnv+"=1")
}

// acceptRunLoggerStderr runs the logger helper as a subprocess and returns the
// logger's actual stderr output.
func acceptRunLoggerStderr(t *testing.T, scenario string, overrides map[string]string) string {
	t.Helper()
	env := make(map[string]string, len(overrides)+1)
	for name, value := range overrides {
		env[name] = value
	}
	env[acceptScenarioEnv] = scenario

	cmd := exec.Command(os.Args[0], "-test.run=^"+acceptHelperName+"$", "-test.timeout=2m")
	cmd.Env = acceptSubprocessEnv(env)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == acceptCallerMutationExit {
			t.Fatal("logger mutated the caller's variadic field slice")
		}
		t.Fatalf("logger subprocess failed: %v\nstderr=%q", err, stderr.String())
	}
	return stderr.String()
}

func acceptStderrLines(t *testing.T, output string) []string {
	t.Helper()
	trimmed := strings.TrimRight(output, "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// acceptRemainderJSON decodes the structured JSON remainder of one console
// line. Lines without a trailing JSON object decode to an empty map.
func acceptRemainderJSON(t *testing.T, line string) map[string]any {
	t.Helper()
	// Encoder-owned SGR sequences may wrap the tail in colored mode. The JSON
	// body itself never contains raw escape bytes, so stripping them first is
	// lossless for the remainder.
	line = acceptANSI.ReplaceAllString(line, "")
	tab := strings.LastIndex(line, "\t")
	if tab < 0 {
		return map[string]any{}
	}
	remainder := line[tab+1:]
	if !strings.HasPrefix(remainder, "{") {
		return map[string]any{}
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(remainder), &decoded); err != nil {
		t.Fatalf("structured remainder is not valid JSON (%q): %v", remainder, err)
	}
	return decoded
}

// acceptAssertContainedOutput pins the injection contract: every hostile log
// call stays one physical line, no unowned control byte reaches stderr, the
// output stays valid UTF-8, and the JSON remainder decodes losslessly. When
// allowColor is set, encoder-owned SGR sequences are expected and stripped
// before the byte checks.
func acceptAssertContainedOutput(t *testing.T, output string, allowColor bool) {
	t.Helper()
	if allowColor {
		if !bytes.Contains([]byte(output), []byte("\x1b[")) {
			t.Fatalf("LOG_COLOR=always produced no ANSI escape sequences: %q", output)
		}
	} else if bytes.Contains([]byte(output), []byte{0x1b}) {
		t.Fatalf("plain mode emitted a raw ANSI escape byte: %q", output)
	}
	raw := acceptANSI.ReplaceAll([]byte(output), nil)
	for _, banned := range [][]byte{{0x1b}, {0x0d}, {0x9b}} {
		if bytes.Contains(raw, banned) {
			t.Fatalf("raw control byte 0x%02x reached stderr: %q", banned[0], output)
		}
	}
	if !utf8.Valid(raw) {
		t.Fatalf("logger stderr is not valid UTF-8: %q", output)
	}
	lines := acceptStderrLines(t, output)
	if len(lines) != 2 {
		t.Fatalf("hostile messages escaped their log lines: %d physical lines\n%q", len(lines), output)
	}
	fields := acceptRemainderJSON(t, lines[1])
	if fields["note"] != acceptJSONNormalizedHostile {
		t.Fatalf("hostile field value was not preserved losslessly: %#v", fields["note"])
	}
	if fields["k\x1bey"] != "val\u2028ue" {
		t.Fatalf("hostile field key or value was not preserved losslessly: %#v", fields)
	}
}

// LOG_COLOR=always must color stderr even when it is a pipe, only emit
// encoder-owned SGR sequences, and carry exactly the same structured content
// as the plain output.
func TestLoggerAcceptanceAlwaysColorsPipedStderr(t *testing.T) {
	plain := acceptRunLoggerStderr(t, "fields", map[string]string{"LOG_COLOR": "never", "TERM": "xterm-256color"})
	colored := acceptRunLoggerStderr(t, "fields", map[string]string{"LOG_COLOR": "always", "TERM": "xterm-256color"})

	if !bytes.Contains([]byte(colored), []byte("\x1b[")) {
		t.Fatalf("LOG_COLOR=always produced no ANSI escape sequences: %q", colored)
	}
	if bytes.Contains(acceptANSI.ReplaceAll([]byte(colored), nil), []byte{0x1b}) {
		t.Fatalf("colored output contains non-SGR escape bytes: %q", colored)
	}

	plainLines := acceptStderrLines(t, plain)
	coloredLines := acceptStderrLines(t, colored)
	if len(plainLines) != 3 || len(coloredLines) != 3 {
		t.Fatalf("expected 3 log lines in both modes, plain=%d colored=%d", len(plainLines), len(coloredLines))
	}
	for index := range plainLines {
		plainFields := acceptRemainderJSON(t, plainLines[index])
		coloredFields := acceptRemainderJSON(t, coloredLines[index])
		if !reflect.DeepEqual(plainFields, coloredFields) {
			t.Fatalf("line %d content differs between plain and colored output:\nplain=%q\ncolored=%q",
				index+1, plainLines[index], coloredLines[index])
		}
	}
}

// Every plain mode — never, auto through a pipe, empty or absent LOG_COLOR,
// and a dumb terminal — must keep ANSI bytes off stderr entirely.
func TestLoggerAcceptancePlainModesNeverEmitANSI(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"never", map[string]string{"LOG_COLOR": "never", "TERM": "xterm-256color"}},
		{"explicit auto through a pipe", map[string]string{"LOG_COLOR": "auto", "TERM": "xterm-256color"}},
		{"empty LOG_COLOR", map[string]string{"LOG_COLOR": "", "TERM": "xterm-256color"}},
		{"absent LOG_COLOR", map[string]string{"TERM": "xterm-256color"}},
		{"dumb terminal with auto", map[string]string{"LOG_COLOR": "auto", "TERM": "dumb"}},
		{"dumb terminal without LOG_COLOR", map[string]string{"TERM": "dumb"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			output := acceptRunLoggerStderr(t, "fields", test.env)
			if bytes.Contains([]byte(output), []byte{0x1b}) {
				t.Fatalf("%s still colored stderr: %q", test.name, output)
			}
			if lines := acceptStderrLines(t, output); len(lines) != 3 {
				t.Fatalf("expected 3 log lines, got %d: %q", len(lines), output)
			}
		})
	}
}

// NO_COLOR forces plain output in every mode, including an empty value under
// LOG_COLOR=always.
func TestLoggerAcceptanceNoColorPresenceForcesPlainEvenUnderAlways(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"empty NO_COLOR under always", map[string]string{"LOG_COLOR": "always", "NO_COLOR": "", "TERM": "xterm-256color"}},
		{"nonempty NO_COLOR without LOG_COLOR", map[string]string{"NO_COLOR": "1", "TERM": "xterm-256color"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			output := acceptRunLoggerStderr(t, "fields", test.env)
			if bytes.Contains([]byte(output), []byte{0x1b}) {
				t.Fatalf("%s ignored NO_COLOR: %q", test.name, output)
			}
			if lines := acceptStderrLines(t, output); len(lines) != 3 {
				t.Fatalf("expected 3 log lines, got %d: %q", len(lines), output)
			}
		})
	}
}

// Values outside auto/always/never must be rejected, not silently ignored.
func TestLoggerAcceptanceRejectsUnknownLogColorPolicy(t *testing.T) {
	t.Setenv("LOG_COLOR", "rainbow")
	if _, err := newLogger(); err == nil || !strings.Contains(err.Error(), "LOG_COLOR") {
		t.Fatalf("unknown LOG_COLOR was not rejected: %v", err)
	}
}

// Named subsystem loggers, account and action IDs, and every structured field
// survive into the JSON remainder, and levels stay distinguishable.
func TestLoggerAcceptanceKeepsNamesAndStructuredFieldsInJSON(t *testing.T) {
	output := acceptRunLoggerStderr(t, "fields", map[string]string{"LOG_COLOR": "never", "TERM": "xterm-256color"})
	lines := acceptStderrLines(t, output)
	if len(lines) != 3 {
		t.Fatalf("expected 3 log lines, got %d: %q", len(lines), output)
	}

	want := map[string]any{
		"account":    "Yukine",
		"account_id": "acc-7",
		"action_id":  "act-42",
		"cargo_tons": float64(120),
		"execute":    true,
	}
	first := acceptRemainderJSON(t, lines[0])
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("structured fields were not preserved in the JSON remainder:\ngot  %#v\nwant %#v", first, want)
	}
	if !strings.Contains(lines[0], "executor") || !strings.Contains(lines[0], "dispatch decision") {
		t.Fatalf("subsystem name or message missing from the first line: %q", lines[0])
	}

	if fields := acceptRemainderJSON(t, lines[1]); len(fields) != 0 {
		t.Fatalf("message-only line invented structured context: %#v", fields)
	}
	if !strings.Contains(lines[1], "dashboard ready") {
		t.Fatalf("plain message missing from the second line: %q", lines[1])
	}

	lower := strings.ToLower(lines[2])
	if !strings.Contains(lower, "warn") || !strings.Contains(lines[2], "retention sweep failed") {
		t.Fatalf("warning level or message missing from the third line: %q", lines[2])
	}
}

// Each Clone level keeps exactly its own context, an empty With adds nothing,
// and logging never mutates the caller's variadic field slice.
func TestLoggerAcceptanceClonesIsolateContextWithoutMutatingCallers(t *testing.T) {
	output := acceptRunLoggerStderr(t, "clone", map[string]string{"LOG_COLOR": "never", "TERM": "xterm-256color"})
	lines := acceptStderrLines(t, output)
	if len(lines) != 5 {
		t.Fatalf("expected 5 log lines, got %d: %q", len(lines), output)
	}

	cases := []struct {
		name string
		line string
		want map[string]any
	}{
		{"base", lines[0], map[string]any{"scope": "base", "base_only": "b"}},
		{"derived", lines[1], map[string]any{"scope": "base", "base_only": "b", "derived_only": "d"}},
		{"sibling", lines[2], map[string]any{"scope": "base", "base_only": "b", "sibling_only": "s"}},
		{"empty", lines[3], map[string]any{}},
		{"mutation probe", lines[4], map[string]any{"kept": "v"}},
	}
	for _, test := range cases {
		got := acceptRemainderJSON(t, test.line)
		if !reflect.DeepEqual(got, test.want) {
			t.Fatalf("%s context wrong:\ngot  %#v\nwant %#v", test.name, got, test.want)
		}
	}
}

// Control characters in messages, logger names, field keys, and field values
// cannot forge physical lines or ANSI sequences in plain or colored output.
func TestLoggerAcceptanceControlCharactersCannotInjectLinesOrANSI(t *testing.T) {
	plain := acceptRunLoggerStderr(t, "injection", map[string]string{"LOG_COLOR": "never", "TERM": "xterm-256color"})
	acceptAssertContainedOutput(t, plain, false)

	colored := acceptRunLoggerStderr(t, "injection", map[string]string{"LOG_COLOR": "always", "TERM": "xterm-256color"})
	acceptAssertContainedOutput(t, colored, true)
}

// The existing LOG_LEVEL contract is unchanged: default info, debug opt-in,
// warn filtering, error silence for probes, and rejection of unknown levels.
func TestLoggerAcceptancePreservesLogLevelSelection(t *testing.T) {
	cases := []struct {
		name      string
		level     string
		wantText  string
		notWanted []string
	}{
		{name: "default level is info", level: "", wantText: "info probe", notWanted: []string{"debug probe"}},
		{name: "debug shows every probe", level: "debug", wantText: "debug probe"},
		{
			name:      "warn hides debug and info",
			level:     "warn",
			wantText:  "warn probe",
			notWanted: []string{"info probe", "debug probe"},
		},
		{
			name:      "error silences every probe",
			level:     "error",
			notWanted: []string{"error probe", "warn probe", "info probe", "debug probe"},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			env := map[string]string{"LOG_COLOR": "never", "TERM": "xterm-256color"}
			if test.level != "" {
				env["LOG_LEVEL"] = test.level
			}
			output := acceptRunLoggerStderr(t, "levels", env)
			if test.wantText != "" && !strings.Contains(output, test.wantText) {
				t.Fatalf("LOG_LEVEL=%q dropped expected output %q: %q", test.level, test.wantText, output)
			}
			for _, missing := range test.notWanted {
				if strings.Contains(output, missing) {
					t.Fatalf("LOG_LEVEL=%q leaked %q: %q", test.level, missing, output)
				}
			}
		})
	}

	t.Setenv("LOG_LEVEL", "verbose")
	if _, err := newLogger(); err == nil || !strings.Contains(err.Error(), "LOG_LEVEL") {
		t.Fatalf("unknown LOG_LEVEL was not rejected: %v", err)
	}
}
