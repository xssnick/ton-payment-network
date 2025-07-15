//go:build !js

package log

import (
	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
)

func SetLogger(l zerolog.Logger) {
	zlog.Logger = l
}

func GetLogger() zerolog.Logger {
	return zlog.Logger
}

var (
	Error = zlog.Error
	Warn  = zlog.Warn
	Info  = zlog.Info
	Debug = zlog.Debug
	Trace = zlog.Trace
	Fatal = zlog.Fatal
)
