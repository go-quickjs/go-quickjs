//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

func init() {
	HostJITEnabled = func(r *Runtime) bool { return r.jitEnabled }
}
