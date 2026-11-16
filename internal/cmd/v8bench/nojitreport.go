//go:build !quickjs_jit || android || ios || !((linux && amd64) || (windows && amd64) || (darwin && arm64))

package main

import (
	"fmt"
	"os"

	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// reportStart refuses -jitreport, -jitcode and -jitcompile: a build without the JIT
// has nothing to report.
func reportStart(runtimeOf) {
	if *jitReportFlag || *jitCodeFlag != "" || *jitTimeFlag != 0 {
		fmt.Fprintln(os.Stderr, "v8bench: -jitreport, -jitcode and -jitcompile require a supported build with -tags quickjs_jit")
		os.Exit(1)
	}
}

// Report prints, after each benchmark, the register tier's crossings where
// QJS_REGREPORT asks for them (docs/register-interpreter.md).
func (runtimeOf) Report(string) { vm.RegReport() }
