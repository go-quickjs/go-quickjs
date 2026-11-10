//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

func init() {
	HostJITEnabled = func(r *Runtime) bool { return r.jitEnabled }
	HostJITMemoryLimit = func(r *Runtime) int64 { return int64(r.jitBudget()) }
}
