//go:build !quickjs_jit || android || ios || !((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

func jitLiteralArraysLetGo[_ any](*Runtime) {}
