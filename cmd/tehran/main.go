// Command tehran is the service entrypoint. It does nothing but hand off to the
// cobra command tree and map its error to an exit code; cobra prints the error.
package main

import (
	"os"

	"github.com/koungkub/tehran/internal/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
