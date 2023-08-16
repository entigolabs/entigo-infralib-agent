package common

import (
	"errors"
	"fmt"
)

func ConvStrToLoggingLvl(str string) LoggingLevel {
	switch str {
	case "dev":
		return DevLoggingLvl
	case "prod":
		return ProdLoggingLvl
	default:
		msg := fmt.Sprintf("unsupported logging level: %s", str)
		Logger.Fatal(&PrefixedError{Reason: errors.New(msg)})
	}
	return ProdLoggingLvl
}
