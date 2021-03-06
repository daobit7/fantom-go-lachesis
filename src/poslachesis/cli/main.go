package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/Fantom-foundation/go-lachesis/src/poslachesis/cli/command"
)

func main() {
	app := prepareApp()
	_ = app.Execute()
}

func prepareApp() *cobra.Command {
	app := cobra.Command{
		Use: os.Args[0],
	}

	app.AddCommand(command.Start)
	app.AddCommand(command.ID)
	app.AddCommand(command.Balance)
	app.AddCommand(command.Transfer)
	app.AddCommand(command.Info)
	app.AddCommand(command.LogLevel)

	return &app
}
