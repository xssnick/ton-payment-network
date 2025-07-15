//go:build js

package log

import (
	"encoding/hex"
	"fmt"
	"syscall/js"
	"time"
)

type Level uint8

const (
	LevelTrace Level = iota
	LevelDebug
	LevelInfo
	LevelWarn
	LevelError
	LevelFatal
	LevelPanic
)

type Logger struct{ lvl Level }
type Event struct {
	lvl    Level
	fields map[string]any
}

var global = &Logger{lvl: LevelDebug}

func Fatal() *Event { return global.newEvent(LevelFatal) }
func Error() *Event { return global.newEvent(LevelError) }
func Warn() *Event  { return global.newEvent(LevelWarn) }
func Info() *Event  { return global.newEvent(LevelInfo) }
func Debug() *Event { return global.newEvent(LevelDebug) }
func Trace() *Event { return global.newEvent(LevelTrace) }

func (l *Logger) newEvent(lvl Level) *Event {
	return &Event{lvl: lvl, fields: make(map[string]any, 8)}
}

func (e *Event) Str(k, v string) *Event { e.fields[k] = v; return e }
func (e *Event) Err(err error) *Event {
	if err != nil {
		e.fields["error"] = err.Error()
	}
	return e
}
func (e *Event) Int(k string, v int) *Event         { e.fields[k] = v; return e }
func (e *Event) Int64(k string, v int64) *Event     { e.fields[k] = fmt.Sprintf("%d", v); return e }
func (e *Event) Uint64(k string, v uint64) *Event   { e.fields[k] = fmt.Sprintf("%d", v); return e }
func (e *Event) Uint32(k string, v uint32) *Event   { e.fields[k] = fmt.Sprintf("%d", v); return e }
func (e *Event) Bool(k string, v bool) *Event       { e.fields[k] = v; return e }
func (e *Event) Float64(k string, v float64) *Event { e.fields[k] = fmt.Sprintf("%f", v); return e }
func (e *Event) Time(k string, t time.Time) *Event {
	e.fields[k] = t.Format(time.RFC3339Nano)
	return e
}
func (e *Event) Dur(k string, d time.Duration) *Event { e.fields[k] = d.String(); return e }
func (e *Event) Bytes(k string, b []byte) *Event      { e.fields[k] = hex.EncodeToString(b); return e }

var console = js.Global().Get("console")

func (e *Event) Msg(msg string) {
	method := "log"
	switch e.lvl {
	case LevelError, LevelFatal, LevelPanic:
		method = "error"
	case LevelWarn:
		method = "warn"
	case LevelInfo:
		method = "info"
	case LevelDebug, LevelTrace:
		method = "debug"
	}

	console.Call(method, msg, e.fields)
}

func (e *Event) Msgf(format string, args ...interface{}) {
	e.Msg(fmt.Sprintf(format, args...))
}

func (l *Logger) With() *Event   { return l.newEvent(l.lvl) }
func (e *Event) Logger() *Logger { return global }
