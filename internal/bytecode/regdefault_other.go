//go:build !amd64 && !arm64

package bytecode

// regArch is RegDefault where QJS_REG says nothing: off, until this
// architecture runs register code by native handlers, as amd64 and arm64
// do; the Go handlers alone are slower than the tiers they would replace.
const regArch = false
