//go:build !quickjs_verify

package vm

// verifyShapes is set by the quickjs_verify build tag; see verifyPropCache.
const verifyShapes = false
