//go:build !amd64 && !386

package engine

// The NaN rule nanRule models here is arm64's (nan_other.go).
const (
	ruleX86          = false
	ruleDefaultNaN32 = 0x7fc00000
	ruleDefaultNaN64 = 0x7ff8000000000000
)

// ruleFused reports whether relaxed_madd and relaxed_nmadd fuse here.
func ruleFused() bool { return true }
