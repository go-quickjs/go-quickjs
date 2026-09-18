package stdlib

import (
	"os"
	"runtime"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// OS installs the "os" module: what the host is, rather than what is on it.
//
// Everything here describes the machine -- its name, its architecture, how many
// processors it has -- which is worth knowing and worth withholding: a host
// that has confined a script to a directory may not want to tell it the user's
// home directory or the machine's name. Each answer is what the host passes,
// and an empty one is answered with an empty string.
type OSInfo struct {
	// Hostname, Homedir and Tmpdir are answered as given. Empty means the
	// script is not told.
	Hostname string
	Homedir  string
	Tmpdir   string
	// Real fills the rest in from the machine itself, which is what a command
	// line tool wants and untrusted code should not have.
	Real bool
}

// System installs the "os" module.
func System(rt *quickjs.Runtime, cfg *OSInfo) error {
	if cfg == nil {
		cfg = &OSInfo{}
	}
	hostname, homedir, tmpdir := cfg.Hostname, cfg.Homedir, cfg.Tmpdir
	if cfg.Real {
		if hostname == "" {
			hostname, _ = os.Hostname()
		}
		if homedir == "" {
			homedir, _ = os.UserHomeDir()
		}
		if tmpdir == "" {
			tmpdir = os.TempDir()
		}
	}

	exports := map[string]any{
		"EOL":      "\n",
		"platform": func() string { return goosToPlatform(runtime.GOOS) },
		"arch":     func() string { return runtime.GOARCH },
		"type": func() string {
			switch runtime.GOOS {
			case "darwin":
				return "Darwin"
			case "windows":
				return "Windows_NT"
			case "linux":
				return "Linux"
			default:
				return runtime.GOOS
			}
		},
		"cpus":     func() float64 { return float64(runtime.NumCPU()) },
		"hostname": func() string { return hostname },
		"homedir":  func() string { return homedir },
		"tmpdir":   func() string { return tmpdir },
	}
	exports["default"] = copyExports(exports)
	if err := rt.SetModule("os", exports); err != nil {
		return err
	}
	return rt.SetModule("node:os", exports)
}
