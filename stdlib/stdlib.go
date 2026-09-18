package stdlib

import (
	"io"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// Config says which capabilities a runtime is given and on what terms.
//
// The zero value installs the things that reach nothing outside the process:
// the console has nowhere to write, and that is all. Everything that can touch
// the world -- a filesystem, the network, the environment -- is a field a host
// has to fill in, so that reading the call says what the script can do.
type Config struct {
	// Stdout and Stderr are where the console writes. Nil discards.
	Stdout, Stderr io.Writer

	// Loop runs timers and the work that finishes on other goroutines. Without
	// one there are no timers, and the promise APIs settle before they return.
	Loop *Loop

	// FS gives the script a filesystem, confined to a root if the host says so.
	// Nil means none: the fs module is not installed at all.
	FS *FS

	// Process describes the process the script thinks it is running in. Nil
	// installs nothing.
	Process *Process

	// OS describes the machine. Nil installs nothing.
	OS *OSInfo

	// Fetch gives the script the network. Nil means none.
	Fetch *Fetch

	// Run lets the script start programs, which is the largest capability
	// there is: a program can do anything the user can. Nil means none.
	Run *Run

	// Serve lets the script listen for HTTP requests, which is the other half
	// of network access and not the same half. Nil means none.
	Serve *Serve

	// Random is where crypto draws its entropy from. Nil uses the system
	// source, which is what a program wants and a test may not.
	Random io.Reader

	// NoWebAPIs leaves out the things a browser has and a language does not --
	// URL, TextEncoder, TextDecoder, structuredClone, performance, crypto,
	// atob, btoa -- and the node modules that need no capability either:
	// events, util, assert and buffer. All of it is pure computation, and all
	// of it is installed by default.
	NoWebAPIs bool
}

// Install gives a runtime everything the configuration asks for.
//
// A host that wants one thing calls the one function for it; this is for the
// host that wants a familiar environment and has decided what that should
// include:
//
//	loop := stdlib.NewLoop(rt)
//	err := stdlib.Install(rt, stdlib.Config{
//	    Stdout:  os.Stdout,
//	    Stderr:  os.Stderr,
//	    Loop:    loop,
//	    FS:      &stdlib.FS{Root: "/srv/data", ReadOnly: true},
//	    Process: &stdlib.Process{Args: os.Args},
//	})
func Install(rt *quickjs.Runtime, cfg Config) error {
	if err := Console(rt, cfg.Stdout, cfg.Stderr); err != nil {
		return err
	}
	if cfg.Loop != nil {
		if err := Timers(rt, cfg.Loop); err != nil {
			return err
		}
	}
	if !cfg.NoWebAPIs {
		if err := WebAPIs(rt, cfg.Random); err != nil {
			return err
		}
		// The node modules that need no capability go with them: an
		// EventEmitter is a list of functions and a Buffer is bytes.
		if err := NodeModules(rt); err != nil {
			return err
		}
	}
	if err := Path(rt); err != nil {
		return err
	}
	if cfg.FS != nil {
		if cfg.FS.Loop == nil {
			cfg.FS.Loop = cfg.Loop
		}
		if err := Files(rt, cfg.FS); err != nil {
			return err
		}
	}
	if cfg.Process != nil {
		// process.stdout and the console write to the same places unless the
		// host has said otherwise: they are the same two streams.
		if cfg.Process.Stdout == nil {
			cfg.Process.Stdout = cfg.Stdout
		}
		if cfg.Process.Stderr == nil {
			cfg.Process.Stderr = cfg.Stderr
		}
		if err := Processes(rt, cfg.Process); err != nil {
			return err
		}
	}
	if cfg.OS != nil {
		if err := System(rt, cfg.OS); err != nil {
			return err
		}
	}
	if cfg.Fetch != nil {
		if cfg.Fetch.Loop == nil {
			cfg.Fetch.Loop = cfg.Loop
		}
		if err := Network(rt, cfg.Fetch); err != nil {
			return err
		}
	}
	if cfg.Serve != nil {
		if cfg.Serve.Loop == nil {
			cfg.Serve.Loop = cfg.Loop
		}
		if err := Servers(rt, cfg.Serve); err != nil {
			return err
		}
	}
	if cfg.Run != nil {
		if cfg.Run.Loop == nil {
			cfg.Run.Loop = cfg.Loop
		}
		if err := Commands(rt, cfg.Run); err != nil {
			return err
		}
	}
	return nil
}
