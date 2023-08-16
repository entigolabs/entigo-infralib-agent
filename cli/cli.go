package cli

import (
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/urfave/cli/v2"
	"os"
	"strings"
)

var flags = new(common.Flags)

func Run() {
	app := &cli.App{Commands: cliCommands()}
	addAppInfo(app)
	loggingLvl := getLoggingLvl()
	common.ChooseLogger(loggingLvl)
	err := app.Run(os.Args)
	if err != nil {
		common.Logger.Fatal(&common.PrefixedError{Reason: err})
	}
}

func addAppInfo(app *cli.App) {
	const agent = "ei-agent"
	app.Name = agent
	app.HelpName = agent
	app.Usage = "entigo infralib agent"
}

func getLoggingLvl() common.LoggingLevel {
	for i, arg := range os.Args {
		if isLoggerFlag(arg) {
			return getLoggerFlagValue(i, arg)
		}
	}
	return common.ProdLoggingLvl
}

func isLoggerFlag(arg string) bool {
	isLongLoggingFlag := strings.Contains(arg, "--logging=") || arg == "--logging"
	isShortLoggingFlag := strings.Contains(arg, "-l=") || arg == "-l"
	return isLongLoggingFlag || isShortLoggingFlag
}

func getLoggerFlagValue(index int, loggerArg string) common.LoggingLevel {
	if strings.Contains(loggerArg, "=") {
		splits := strings.Split(loggerArg, "=")
		loggingLvlAsString := strings.TrimSpace(splits[len(splits)-1])
		return common.ConvStrToLoggingLvl(loggingLvlAsString)
	} else {
		return common.ConvStrToLoggingLvl(os.Args[index+1])
	}
}
