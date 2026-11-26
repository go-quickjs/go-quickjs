//go:build !amd64 && !arm64

package opt

import wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"

// genScratch is the lowering's part of a workspace: none without a
// lowering.
type genScratch struct{}

// hasSIMD reports whether the code may take SIMD: no code is made here.
func hasSIMD() bool { return false }

// cpuFeatures are the CPU's features the code may use: none here.
func cpuFeatures() wjit.Features { return wjit.Features{} }
