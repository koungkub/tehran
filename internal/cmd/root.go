// Package cmd defines the tehran CLI commands.
package cmd

import (
	"github.com/spf13/cobra"
)

// Execute runs the root command; main() maps its error to the exit code.
func Execute() error {
	return newRootCommand().Execute()
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:          "tehran",
		Short:        "tehran is a ConnectRPC service",
		SilenceUsage: true,
	}
	root.PersistentFlags().String("config", "", "path to a TOML config file (default: ./config.toml, /etc/tehran/config.toml)")
	root.AddCommand(newAPICommand(), newVersionCommand())
	return root
}
