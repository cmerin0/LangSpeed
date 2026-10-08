// Package logging emits the structured logs required by Requirement 21.
//
// Every line is a single JSON object with a timestamp, a severity level
// (DEBUG, INFO, WARN, ERROR) and arbitrary key/value fields, so lines can be
// parsed and filtered programmatically (R21.10).
package logging

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

// Level is the severity of a log line (glossary: Log_Level).
type Level string

// The four log levels defined in the glossary.
const (
	Debug Level = "DEBUG"
	Info  Level = "INFO"
	Warn  Level = "WARN"
	Error Level = "ERROR"
)

// Logger writes structured JSON lines to an output writer. It is safe for
// concurrent use.
type Logger struct {
	mu  sync.Mutex
	out io.Writer
}

// New returns a logger writing to out (stdout when out is nil).
func New(out io.Writer) *Logger {
	if out == nil {
		out = os.Stdout
	}
	return &Logger{out: out}
}

// Log writes one structured line at the given level. Fields are key/value
// pairs: log.Info("message", "key1", value1, "key2", value2).
func (l *Logger) Log(level Level, msg string, kv ...any) {
	record := map[string]any{
		"time":  time.Now().UTC().Format(time.RFC3339Nano),
		"level": string(level),
		"msg":   msg,
	}
	for i := 0; i+1 < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			continue
		}
		record[key] = kv[i+1]
	}
	line, err := json.Marshal(record)
	if err != nil {
		// Never lose the event itself, even if a field cannot be encoded.
		line = []byte(`{"level":"ERROR","msg":"log record could not be encoded"}`)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.out.Write(append(line, '\n'))
}

// Debug logs at DEBUG level.
func (l *Logger) Debug(msg string, kv ...any) { l.Log(Debug, msg, kv...) }

// Info logs at INFO level.
func (l *Logger) Info(msg string, kv ...any) { l.Log(Info, msg, kv...) }

// Warn logs at WARN level.
func (l *Logger) Warn(msg string, kv ...any) { l.Log(Warn, msg, kv...) }

// Error logs at ERROR level.
func (l *Logger) Error(msg string, kv ...any) { l.Log(Error, msg, kv...) }
