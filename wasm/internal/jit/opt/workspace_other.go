//go:build !amd64 && !arm64

package opt

// genScratch is the lowering's part of a workspace: none without a
// lowering.
type genScratch struct{}

// hasSIMD reports whether the code may take SIMD: no code is made here.
func hasSIMD() bool { return false }
