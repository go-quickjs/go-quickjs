//go:build quickjs_jit && !android && !ios

package jit

// Linux maps code read and write, then read and execute (memory_unix.go),
// with no policy of its own to ask; the caches are maintained by
// flushInstructionCache (flush_linux_arm64.s).
func executablePolicy() error { return nil }
