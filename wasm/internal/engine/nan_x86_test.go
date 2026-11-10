//go:build amd64 || 386

package engine

// The NaN rule nanRule models here is amd64's and 386's (nan_x86.go).
const (
	ruleX86          = true
	ruleDefaultNaN32 = 0xffc00000
	ruleDefaultNaN64 = 0xfff8000000000000
)

// ruleFused reports whether relaxed_madd and relaxed_nmadd fuse here.
func ruleFused() bool { return relaxedFused }
