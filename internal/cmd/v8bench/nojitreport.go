//go:build !quickjs_jit || android || ios || !((linux && amd64) || (windows && amd64) || (darwin && arm64))

package main

import (
	"fmt"
	"os"
)

// reportStart refuses -jitreport: a build without the JIT has nothing to
// report.
func reportStart(runtimeOf) {
	if *jitReportFlag {
		fmt.Fprintln(os.Stderr, "v8bench: -jitreport requires a supported build with -tags quickjs_jit")
		os.Exit(1)
	}
}
