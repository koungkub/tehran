package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/koungkub/tehran/internal/platform/version"
)

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), version.String())
		},
	}
}
