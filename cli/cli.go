package cli

import (
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/urfave/cli/v2"
	"log"
	"os"
)

var flags = new(common.Flags)

func Run() {
	app := &cli.App{Commands: cliCommands()}
	addAppInfo(app)
	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(&common.PrefixedError{Reason: err})
	}
}

func addAppInfo(app *cli.App) {
	const agent = "ei-agent"
	app.Name = agent
	app.HelpName = agent
	app.Usage = "entigo infralib agent"
}
