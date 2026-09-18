package stdlib

import (
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// Process describes what the process object tells a script about the world it
// is running in.
//
// The zero value tells it almost nothing: no arguments, no environment, and an
// exit that does nothing. That is deliberate -- the environment of a process is
// a pile of secrets, and a script should be given the parts of it the host
// meant to give.
type Process struct {
	// Args are the command line the script sees as process.argv. The first two
	// entries are conventionally the executable and the script, which is what a
	// program that reads argv.slice(2) expects.
	Args []string
	// Env is the environment, which is empty unless the host fills it in.
	// Passing os.Environ() as a map hands over everything, secrets included.
	Env map[string]string
	// Cwd is what process.cwd() answers. Empty answers "/", which is what a
	// runtime with no filesystem has.
	Cwd string
	// Stdout and Stderr are where process.stdout.write and its partner go.
	Stdout, Stderr io.Writer
	// Stdin is what process.stdin.read() draws from, read in full on first use.
	Stdin io.Reader
	// Exit is called by process.exit. A host that wants the script to be able
	// to end the program passes os.Exit; one that does not leaves it nil, and
	// the call raises an exception the script can see but not ignore.
	Exit func(code int)
	// Version is what process.version reports.
	Version string
}

// Processes installs the process object, as a global and as the "process"
// module, which is where a program looks for it either way.
func Processes(rt *quickjs.Runtime, cfg *Process) error {
	if cfg == nil {
		cfg = &Process{}
	}
	p := rt.NewObject()

	argv := cfg.Args
	if argv == nil {
		argv = []string{}
	}
	if err := p.Set("argv", argv); err != nil {
		return err
	}
	env := rt.NewObject()
	names := make([]string, 0, len(cfg.Env))
	for k := range cfg.Env {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if err := env.Set(k, cfg.Env[k]); err != nil {
			return err
		}
	}
	if err := p.Set("env", env); err != nil {
		return err
	}

	version := cfg.Version
	if version == "" {
		version = "go-quickjs"
	}
	started := time.Now()
	if err := setAll(p, map[string]any{
		"platform": goosToPlatform(runtime.GOOS),
		"arch":     runtime.GOARCH,
		"version":  version,
		"pid":      float64(os.Getpid()),
		"cwd": func() string {
			if cfg.Cwd == "" {
				return "/"
			}
			return cfg.Cwd
		},
		"uptime": func() float64 { return time.Since(started).Seconds() },
		"hrtime": func(r *quickjs.Runtime, prev quickjs.Value) (quickjs.Value, error) {
			// [seconds, nanoseconds], as node reports it, optionally as the
			// difference from an earlier reading.
			now := time.Since(started)
			if prev.IsArray() && prev.Len() == 2 {
				a, _ := prev.Index(0)
				b, _ := prev.Index(1)
				now -= time.Duration(a.Float())*time.Second + time.Duration(b.Float())
			}
			return r.NewArray(float64(now/time.Second), float64(now%time.Second))
		},
		"exit": func(code quickjs.Value) error {
			n := 0
			if code.Kind() == quickjs.KindNumber {
				n = code.Int()
			}
			if cfg.Exit == nil {
				return rt.Throw(rt.NewError("Error",
					"this runtime cannot end the process"))
			}
			cfg.Exit(n)
			return nil
		},
		// nextTick is a microtask here. Node's runs before promise reactions
		// rather than among them, which is a distinction only node has.
		"nextTick": func(fn quickjs.Value, rest ...quickjs.Value) error {
			if !fn.IsFunction() {
				return rt.Throw(rt.NewError("TypeError", "the argument must be a function"))
			}
			args := make([]any, len(rest))
			for i, v := range rest {
				args[i] = v
			}
			rt.EnqueueJob(func() { fn.Call(args...) })
			return nil
		},
	}); err != nil {
		return err
	}

	stdout, err := writableStream(rt, cfg.Stdout)
	if err != nil {
		return err
	}
	stderr, err := writableStream(rt, cfg.Stderr)
	if err != nil {
		return err
	}
	stdin, err := readableStream(rt, cfg.Stdin)
	if err != nil {
		return err
	}
	if err := setAll(p, map[string]any{
		"stdout": stdout,
		"stderr": stderr,
		"stdin":  stdin,
	}); err != nil {
		return err
	}

	// The event interface a node program expects, as far as it means anything
	// here: "exit" is called when the host says the program is ending, and
	// "unhandledRejection" when a promise nobody took is rejected. Anything
	// else is remembered and never emitted, which is better than refusing to
	// register it.
	events, err := evalWithHost(rt, "<process-events>", processEventsJS, p)
	if err != nil {
		return err
	}
	if err := p.Set("on", mustGet(events, "on")); err != nil {
		return err
	}
	for _, name := range []string{"once", "off", "removeListener", "emit", "listeners"} {
		if err := p.Set(name, mustGet(events, name)); err != nil {
			return err
		}
	}
	rt.OnUnhandledRejection(func(reason quickjs.Value) {
		emit, err := p.Get("emit")
		if err != nil || !emit.IsFunction() {
			return
		}
		emit.CallWithThis(p, "unhandledRejection", reason)
	})

	if err := rt.Set("process", p); err != nil {
		return err
	}
	exports := map[string]any{"default": p}
	if err := rt.SetModule("process", exports); err != nil {
		return err
	}
	return rt.SetModule("node:process", exports)
}

// writableStream is the little of a stream that a script writing output needs.
func writableStream(rt *quickjs.Runtime, w io.Writer) (quickjs.Value, error) {
	o := rt.NewObject()
	err := setAll(o, map[string]any{
		"write": func(s string) bool {
			if w == nil {
				return true
			}
			io.WriteString(w, s)
			return true
		},
		"isTTY": false,
	})
	return o, err
}

// readableStream is the little of a stream that a script reading input needs:
// the whole of it, read when first asked for.
func readableStream(rt *quickjs.Runtime, r io.Reader) (quickjs.Value, error) {
	var contents string
	var read bool
	o := rt.NewObject()
	err := setAll(o, map[string]any{
		"read": func() any {
			if r == nil {
				return nil
			}
			if !read {
				b, _ := io.ReadAll(r)
				contents, read = string(b), true
			}
			if contents == "" {
				return nil
			}
			out := contents
			contents = ""
			return out
		},
		"isTTY": false,
	})
	return o, err
}

func setAll(o quickjs.Value, entries map[string]any) error {
	names := make([]string, 0, len(entries))
	for k := range entries {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if err := o.Set(k, entries[k]); err != nil {
			return err
		}
	}
	return nil
}

// goosToPlatform reports the operating system under the names node uses, since
// that is what a program written for node compares against.
func goosToPlatform(goos string) string {
	switch goos {
	case "darwin":
		return "darwin"
	case "windows":
		return "win32"
	default:
		return strings.ToLower(goos)
	}
}

// mustGet reads a property that the source above certainly defines.
func mustGet(o quickjs.Value, name string) quickjs.Value {
	v, _ := o.Get(name)
	return v
}

// processEventsJS is the event interface of the process object.
const processEventsJS = `(function (process) {
  "use strict";
  const listeners = new Map();
  const add = (name, fn, once) => {
    if (typeof fn !== "function") throw new TypeError("the listener must be a function");
    const list = listeners.get(name) || [];
    list.push({fn, once});
    listeners.set(name, list);
    return process;
  };
  return {
    on: (name, fn) => add(String(name), fn, false),
    once: (name, fn) => add(String(name), fn, true),
    off: remove,
    removeListener: remove,
    listeners: (name) => (listeners.get(String(name)) || []).map(l => l.fn),
    emit(name, ...args) {
      const list = listeners.get(String(name));
      if (!list || list.length === 0) return false;
      for (const l of list.slice()) {
        if (l.once) remove(name, l.fn);
        l.fn.apply(process, args);
      }
      return true;
    },
  };

  function remove(name, fn) {
    const list = listeners.get(String(name));
    if (list) {
      const at = list.findIndex(l => l.fn === fn);
      if (at >= 0) list.splice(at, 1);
    }
    return process;
  }
})`
