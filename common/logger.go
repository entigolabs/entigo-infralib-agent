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

func PrintWarning(message string) {
	warning := Warning{Reason: errors.New(message)}
	slog.Warn(warning.Error())
}

type PrefixedError struct {
	Reason error
}

func (pe *PrefixedError) Error() string {
	return fmt.Sprintf("\x1b[31;1m%s\x1b[0m", pe.Reason)
}

func PrintError(err error) {
	prefixed := PrefixedError{Reason: err}
	slog.Error(prefixed.Error())
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
