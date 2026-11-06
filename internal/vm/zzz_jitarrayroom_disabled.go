//go:build !quickjs_jit || android || ios || !((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

func jitLiteralArraysLetGo[_ any](*Runtime) {}
