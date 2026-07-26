// Package version holds build information injected at link time via
// -ldflags "-X github.com/koungkub/tehran/internal/version.Version=...".
package version

import (
	"fmt"
	"runtime"
)

// Build stamps, overwritten at link time. The defaults are what a plain
// `go build` produces.
var (
	Version   = "dev"
	GitCommit = "none"
	BuildDate = "unknown"
)

// String renders the build stamp together with the toolchain and target.
func String() string {
	return fmt.Sprintf("tehran %s (commit %s, built %s, %s %s/%s)",
		Version, GitCommit, BuildDate, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
