// Package version holds build information injected at link time via
// -ldflags "-X github.com/koungkub/tehran/internal/platform/version.Version=...".
package version

import (
	"fmt"
	"runtime"
)

var (
	Version   = "dev"
	GitCommit = "none"
	BuildDate = "unknown"
)

func String() string {
	return fmt.Sprintf("tehran %s (commit %s, built %s, %s %s/%s)",
		Version, GitCommit, BuildDate, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
