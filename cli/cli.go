package cli

import (
	"context"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/urfave/cli/v3"
	"log"
	"os"
)

var flags = new(common.Flags)

func Run(ctx context.Context) {
	app := &cli.Command{Commands: cliCommands()}
	addAppInfo(app)
	err := app.Run(ctx, os.Args)
	if err != nil {
		log.Fatal(&common.PrefixedError{Reason: err})
	}
}

func addAppInfo(app *cli.Command) {
	const agent = "ei-agent"
	app.Name = agent
	app.Usage = "entigo infralib agent"
}
