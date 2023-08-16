package common

import (
	"errors"
	"fmt"
	"log"
	"os"
)

var prodLogger = log.New(os.Stderr, "", 0)
var Logger = prodLogger

type Warning struct {
	Reason error
}

func (w *Warning) Error() string {
	warning := fmt.Sprintf("[warning] %s", w.Reason)
	return fmt.Sprintf("\x1b[36;1m%s\x1b[0m", warning)
}

func PrintWarning(message string) {
	warning := Warning{
		Reason: errors.New(message),
	}
	Logger.Println(warning.Error())
}

type PrefixedError struct {
	Reason error
}

func (pe *PrefixedError) Error() string {
	err := fmt.Sprintf("[error] %s", pe.Reason)
	return fmt.Sprintf("\x1b[31;1m%s\x1b[0m", err)
}

func PrintError(err error) {
	prefixed := PrefixedError{Reason: err}
	Logger.Println(prefixed.Error())
}

func ChooseLogger(loggingLvl LoggingLevel) {
	switch loggingLvl {
	case DevLoggingLvl:
		Logger = log.New(os.Stderr, "gitops: ", log.LstdFlags|log.Lshortfile)
	case ProdLoggingLvl:
		Logger = prodLogger
	default:
		msg := fmt.Sprintf("unsupported logging level: %v", loggingLvl)
		Logger.Fatal(&PrefixedError{errors.New(msg)})
	}
}
