//go:build !quickjs_verify

package ssa

// verifyLiveness is set by the quickjs_verify build tag (liveverify_on.go).
const verifyLiveness = false

func liveAcrossRef(*Func) ([]*Value, [][]*Value) { panic("ssa: liveAcrossRef without quickjs_verify") }
