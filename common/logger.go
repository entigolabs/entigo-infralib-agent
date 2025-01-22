package common

import (
	"errors"
	"fmt"
	"log"
	"log/slog"
)

type Warning struct {
	Reason error
}

func (w *Warning) Error() string {
	return fmt.Sprintf("\x1b[36;1m%s\x1b[0m", w.Reason)
}

func PrefixWarning(message string) string {
	warning := Warning{Reason: errors.New(message)}
	return warning.Error()
}

type PrefixedError struct {
	Reason error
}

func (pe *PrefixedError) Error() string {
	return fmt.Sprintf("\x1b[31;1m%s\x1b[0m", pe.Reason)
}

func PrefixError(err error) string {
	prefixed := PrefixedError{Reason: err}
	return prefixed.Error()
}

func ChooseLogger(loggingLvl string) {
	switch loggingLvl {
	case string(DebugLogLevel):
		slog.SetLogLoggerLevel(slog.LevelDebug)
		log.SetFlags(log.LstdFlags | log.Lshortfile)
	case string(WarnLogLevel):
		slog.SetLogLoggerLevel(slog.LevelWarn)
		fallthrough
	case string(ErrorLogLevel):
		slog.SetLogLoggerLevel(slog.LevelError)
		fallthrough
	case string(ProdLogLevel):
		log.SetFlags(0)
	default:
		msg := fmt.Sprintf("unsupported logging level: %v", loggingLvl)
		log.Fatal(&PrefixedError{errors.New(msg)})
	}
}
