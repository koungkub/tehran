package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	dbapp "github.com/koungkub/tehran/internal/app/db"
	"github.com/koungkub/tehran/internal/config"
	"github.com/koungkub/tehran/pkg/migrate"
)

func newDBCommand() *cobra.Command {
	db := &cobra.Command{
		Use:   "db",
		Short: "Manage the service database schema",
		Long: "Manage the service database schema.\n\n" +
			"Migrations are embedded in this binary, so the image being deployed carries\n" +
			"exactly the schema it expects. Run `db migrate` as a step the rollout is gated\n" +
			"on rather than from the api server: the server has N replicas starting at once,\n" +
			"and its role should not be allowed to ALTER anything.",
	}
	db.AddCommand(newDBMigrateCommand(), newDBStatusCommand(), newDBSchemaVersionCommand())
	return db
}

func newDBMigrateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply pending schema migrations",
		Long: "Apply pending schema migrations, in version order.\n\n" +
			"Concurrent runners are held apart by [migrate].lock_mode, so two of these\n" +
			"starting together is safe on PostgreSQL: the second waits out the first.\n" +
			"A migration that fails leaves the ones before it applied, and those are\n" +
			"reported before the error.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			to, err := cmd.Flags().GetInt64("to")
			if err != nil {
				return err
			}
			// 0 is the default and means "everything pending". A negative value is
			// not a version, and silently treating it as 0 would apply every
			// migration to somebody who meant to bound the run.
			if to < 0 {
				return fmt.Errorf("--to %d is not a version", to)
			}
			return withDBApp(cmd, func(ctx context.Context, app *dbapp.App) error {
				applied, err := app.Migrate(ctx, to)
				// Reported before the error, not instead of it: a partial run is
				// exactly when what did land matters most. A run that applied
				// nothing and failed says nothing here, though — "no pending
				// migrations" above an error is a straight contradiction.
				if err == nil || len(applied) > 0 {
					printApplied(cmd.OutOrStdout(), applied)
				}
				return err
			})
		},
	}
	cmd.Flags().Int64("to", 0, "stop at this version instead of applying every pending migration (0 applies all)")
	return cmd
}

func newDBStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "List every migration and whether it has been applied",
		Long: "List every migration and whether it has been applied.\n\n" +
			"It takes no migration lock, so it reports on a run that is in progress\n" +
			"rather than queueing behind it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDBApp(cmd, func(ctx context.Context, app *dbapp.App) error {
				statuses, err := app.Status(ctx)
				if err != nil {
					return err
				}
				printStatus(cmd.OutOrStdout(), statuses)
				return nil
			})
		},
	}
}

// newDBSchemaVersionCommand is `db version`, distinct from the top-level `version`
// command that prints the build stamp — hence the name, which cobra never shows.
func newDBSchemaVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the schema version recorded in the database",
		Long: "Print the schema version recorded in the database.\n\n" +
			"Exits non-zero when migrations are pending, which is what makes it usable as\n" +
			"a gate: a rollout that must not run ahead of its schema can check this first.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDBApp(cmd, func(ctx context.Context, app *dbapp.App) error {
				version, pending, err := app.Version(ctx)
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				_, _ = fmt.Fprintln(out, version)
				if pending {
					return errors.New("migrations are pending")
				}
				return nil
			})
		},
	}
}

// withDBApp loads the configuration, wires the db command's application, and
// closes it again once fn returns.
//
// fn's context is cancelled by SIGINT or SIGTERM. That matters more here than for
// a one-shot command in general: each migration runs in a transaction, so a
// cancelled statement rolls its migration back cleanly, whereas the SIGKILL that
// follows an ignored SIGTERM leaves the connection to be reaped by the server. A
// migration marked NO TRANSACTION is the exception and cannot be protected either
// way.
func withDBApp(cmd *cobra.Command, fn func(context.Context, *dbapp.App) error) (err error) {
	configFile, err := cmd.Flags().GetString("config")
	if err != nil {
		return err
	}
	cfg, err := config.Load(configFile, cmd.Flags())
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := dbapp.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, app.Close()) }()
	return fn(ctx, app)
}

func printApplied(out io.Writer, applied []migrate.Applied) {
	if len(applied) == 0 {
		_, _ = fmt.Fprintln(out, "no pending migrations")
		return
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "VERSION\tDIRECTION\tDURATION\tSOURCE")
	for _, a := range applied {
		source := a.Source
		if a.Empty {
			// Worth saying: a version recorded with no statements behind it looks
			// identical to a real one in the version table.
			source += " (empty)"
		}
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", a.Version, a.Direction, a.Duration.Round(time.Millisecond), source)
	}
	_ = w.Flush()
}

func printStatus(out io.Writer, statuses []migrate.Status) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "VERSION\tAPPLIED AT\tSOURCE")
	for _, s := range statuses {
		when := "pending"
		if s.Applied {
			when = s.AppliedAt.Format(time.RFC3339)
		}
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\n", s.Version, when, s.Source)
	}
	_ = w.Flush()
}
