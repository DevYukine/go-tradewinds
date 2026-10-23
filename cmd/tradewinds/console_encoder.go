package main

import (
	"unicode"
	"unicode/utf8"

	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

const (
	consoleReset  = "\x1b[0m"
	consoleDim    = "\x1b[2m"
	consoleGray   = "\x1b[90m"
	consoleWhite  = "\x1b[37m"
	consoleCyan   = "\x1b[36m"
	consoleGreen  = "\x1b[32m"
	consoleYellow = "\x1b[33m"
	consoleRed    = "\x1b[31m"
)

var consoleBuffers = buffer.NewPool()

// The JSON delegate owns all structured fields; the account bracket is only
// an additional presentation of its top-level account field.
type consoleEncoder struct {
	zapcore.Encoder
	color      bool
	account    string
	namespaced bool
}

func newConsoleEncoder(color bool) zapcore.Encoder {
	return &consoleEncoder{
		Encoder: zapcore.NewJSONEncoder(zapcore.EncoderConfig{
			CallerKey:      "caller",
			FunctionKey:    "function",
			StacktraceKey:  "stacktrace",
			SkipLineEnding: true,
			EncodeTime:     zapcore.ISO8601TimeEncoder,
			EncodeDuration: zapcore.StringDurationEncoder,
			EncodeCaller:   zapcore.ShortCallerEncoder,
		}),
		color: color,
	}
}

func (e *consoleEncoder) Clone() zapcore.Encoder {
	return &consoleEncoder{
		Encoder:    e.Encoder.Clone(),
		color:      e.color,
		account:    e.account,
		namespaced: e.namespaced,
	}
}

func (e *consoleEncoder) AddString(key, value string) {
	if key == "account" && !e.namespaced {
		e.account = value
	}
	e.Encoder.AddString(key, value)
}

func (e *consoleEncoder) OpenNamespace(key string) {
	e.namespaced = true
	e.Encoder.OpenNamespace(key)
}

func (e *consoleEncoder) EncodeEntry(entry zapcore.Entry, fields []zapcore.Field) (*buffer.Buffer, error) {
	context, err := e.Encoder.EncodeEntry(entry, fields)
	if err != nil {
		return nil, err
	}
	defer context.Free()

	account := e.account
	namespaced := e.namespaced
	for _, field := range fields {
		if field.Type == zapcore.NamespaceType {
			namespaced = true
		}
		if !namespaced && field.Key == "account" && field.Type == zapcore.StringType {
			account = field.String
		}
	}

	line := consoleBuffers.Get()
	if e.color {
		line.AppendString(consoleGray)
	}
	line.AppendByte('[')
	line.AppendTime(entry.Time, "15:04:05")
	line.AppendByte(']')
	if e.color {
		line.AppendString(consoleReset)
	}
	if account != "" {
		line.AppendByte(' ')
		e.appendBracket(line, consoleWhite, account)
	}
	category := entry.LoggerName
	if category == "" {
		category = "root"
	}
	line.AppendByte(' ')
	e.appendBracket(line, consoleCyan, category)
	line.AppendByte(' ')
	e.appendBracket(line, consoleLevelColor(entry.Level), entry.Level.CapitalString())
	line.AppendByte(' ')
	appendConsoleEscaped(line, entry.Message, utf8.DecodeRuneInString, (*buffer.Buffer).AppendString)
	if context.Len() > 2 {
		line.AppendByte('\t')
		if e.color {
			line.AppendString(consoleDim)
		}
		appendConsoleEscaped(line, context.Bytes(), utf8.DecodeRune, (*buffer.Buffer).AppendBytes)
		if e.color {
			line.AppendString(consoleReset)
		}
	}
	line.AppendByte('\n')
	return line, nil
}

func (e *consoleEncoder) appendBracket(line *buffer.Buffer, color, text string) {
	if e.color {
		line.AppendString(color)
	}
	line.AppendByte('[')
	appendConsoleEscaped(line, text, utf8.DecodeRuneInString, (*buffer.Buffer).AppendString)
	line.AppendByte(']')
	if e.color {
		line.AppendString(consoleReset)
	}
}

func consoleLevelColor(level zapcore.Level) string {
	switch {
	case level >= zapcore.ErrorLevel:
		return consoleRed
	case level == zapcore.WarnLevel:
		return consoleYellow
	case level == zapcore.DebugLevel:
		return consoleGray
	default:
		return consoleGreen
	}
}

// Escape after JSON encoding so objects, errors, reflection, duplicate keys
// and namespaces keep Zap's semantics. JSON-compatible escapes also keep the
// remainder decodable without copying or re-marshalling its structured data.
func appendConsoleEscaped[S string | []byte](line *buffer.Buffer, text S, decode func(S) (rune, int), appendText func(*buffer.Buffer, S)) {
	last := 0
	for offset := 0; offset < len(text); {
		r, width := decode(text[offset:])
		if (r == utf8.RuneError && width == 1) || unicode.IsControl(r) ||
			unicode.Is(unicode.Bidi_Control, r) || r == '\u2028' || r == '\u2029' {
			appendText(line, text[last:offset])
			const hex = "0123456789abcdef"
			line.AppendString(`\u`)
			line.AppendByte(hex[(r>>12)&0xf])
			line.AppendByte(hex[(r>>8)&0xf])
			line.AppendByte(hex[(r>>4)&0xf])
			line.AppendByte(hex[r&0xf])
			offset += width
			last = offset
			continue
		}
		offset += width
	}
	appendText(line, text[last:])
}
