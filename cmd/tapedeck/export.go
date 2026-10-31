package main

import (
	"github.com/echotools/tape/v4/pkg/conversion"
	"github.com/spf13/cobra"
)

func newExportCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export a tape to another format",
	}
	cmd.AddCommand(newExportEchoreplayCommand())
	return cmd
}

func newExportEchoreplayCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "echoreplay <in.tape> <out.echoreplay>",
		Short: "Reconstruct a tape as an echoreplay file",
		Long: `Reconstruct each frame of a tape and write it as an echoreplay file:
a zip holding one tab-separated line per frame, timestamp, then sessionJSON.

Prints the number of frames written. The output is only as complete as the
reconstruction: fields the tape does not carry are not reconstructed.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			written, err := conversion.ReconstructFile(args[0], args[1])
			if err != nil {
				return err
			}
			printf(cmd.OutOrStdout(), "wrote %d frame(s) to %s\n", written, args[1])
			return nil
		},
	}
}
