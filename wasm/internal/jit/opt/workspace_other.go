//go:build !amd64 && !arm64

package opt

// genScratch is the lowering's part of a workspace: none without a
// lowering.
type genScratch struct{}
