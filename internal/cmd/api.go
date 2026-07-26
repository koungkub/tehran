package cmd

import (
	"github.com/spf13/cobra"

	"github.com/koungkub/tehran/internal/app/api"
	"github.com/koungkub/tehran/internal/config"
)

func newAPICommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "api",
		Short: "Run the ConnectRPC API server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			configFile, err := cmd.Flags().GetString("config")
			if err != nil {
				return err
			}
			cfg, err := config.Load(configFile, cmd.Flags())
			if err != nil {
				return err
			}
			application, err := api.New(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			return application.Run(cmd.Context())
		},
	}

	// Only host and port are exposed as CLI flags; every other setting comes
	// from the TOML config file or TEHRAN_* environment variables.
	// Flag defaults mirror config.Load defaults for --help display only;
	// a flag overrides env/file/default only when actually passed (pflag.Changed).
	flags := cmd.Flags()
	flags.String("host", "0.0.0.0", "address the RPC server listens on")
	flags.Int("port", 8080, "port the RPC server listens on")
	return cmd
}
