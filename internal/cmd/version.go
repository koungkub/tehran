package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/koungkub/tehran/internal/version"
)

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		Run: func(cmd *cobra.Command, _ []string) {
			// Nothing useful to do if stdout is gone.
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), version.String())
		},
	}
}
