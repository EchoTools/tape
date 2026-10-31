package main

import (
	"os"

	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "tapedeck",
		Short: "Tape format tools for Echo VR telemetry",
	}
	root.AddCommand(newConvertCommand())
	root.AddCommand(newShowCommand())
	root.AddCommand(newReplayCommand())
	root.AddCommand(newVerifyCommand())
	root.AddCommand(newStatsCommand())
	root.AddCommand(newDiffCommand())
	root.AddCommand(newTrimCommand())
	root.AddCommand(newExportCommand())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
