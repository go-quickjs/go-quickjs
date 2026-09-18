package stdlib

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// Run describes the programs a script may start.
//
// This is the largest capability there is: a script that can run a program can
// do anything the user can, whatever else it was refused. The zero value allows
// nothing, and Allow decides for every attempt:
//
//	stdlib.Commands(rt, &stdlib.Run{
//	    Loop:  loop,
//	    Allow: func(name string, args []string) error {
//	        if name != "git" {
//	            return fmt.Errorf("only git may be run")
//	        }
//	        return nil
//	    },
//	})
type Run struct {
	// Loop is where the result is delivered for the promise form. Without one
	// the work is done before the call returns.
	Loop *Loop
	// Allow is asked about every program before it is started. Nil refuses
	// everything, which is what a zero value means.
	Allow func(name string, args []string) error
	// Dir is where a program runs when the caller does not say. Empty means
	// the process's own directory.
	Dir string
	// Env is the environment a program is given. Nil means none at all, which
	// is what a script that was not given the environment should pass on.
	Env map[string]string
	// Timeout stops a program that runs too long. Zero means no limit.
	Timeout time.Duration
	// MaxOutputBytes caps what is captured from a program. Zero means 32 MB.
	MaxOutputBytes int
}

// Commands installs the "child_process" module.
//
// What is there is what can be done without streams: a program is started, it
// runs to completion, and its output comes back whole.
//
//	import {execFileSync, execFile} from "child_process"
//	const branch = execFileSync("git", ["branch", "--show-current"]).trim()
//	const {stdout} = await execFile("git", ["status", "--short"])
func Commands(rt *quickjs.Runtime, cfg *Run) error {
	if cfg == nil {
		cfg = &Run{}
	}
	r := &runner{rt: rt, cfg: cfg}

	exports := map[string]any{
		"execFileSync": r.execFileSync,
		"spawnSync":    r.spawnSync,
		"execFile":     r.execFile,
		"exec": func(command string, opts quickjs.Value) *quickjs.Promise {
			name, args := shellFor(command)
			return r.execFile(name, quickjs.Value{}, opts, args...)
		},
		"execSync": func(command string, opts quickjs.Value) (string, error) {
			name, args := shellFor(command)
			return r.execFileSync(name, quickjs.Value{}, opts, args...)
		},
	}
	exports["default"] = copyExports(exports)
	if err := rt.SetModule("child_process", exports); err != nil {
		return err
	}
	return rt.SetModule("node:child_process", exports)
}

// shellFor is how a command line is run: through the shell, which is what
// makes pipes and redirection in the string work.
func shellFor(command string) (string, []string) {
	return "/bin/sh", []string{"-c", command}
}

type runner struct {
	rt  *quickjs.Runtime
	cfg *Run
}

// result is a finished program.
type result struct {
	stdout []byte
	stderr []byte
	code   int
	err    error
}

// start runs a program to completion. It touches no JavaScript value, so it
// may run on another goroutine.
func (r *runner) start(name string, args []string, dir string, env map[string]string, input []byte) *result {
	if r.cfg.Allow == nil {
		return &result{err: errors.New("running programs is not allowed")}
	}
	if err := r.cfg.Allow(name, args); err != nil {
		return &result{err: err}
	}

	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if cmd.Dir == "" {
		cmd.Dir = r.cfg.Dir
	}
	// A program is given the environment the host chose, not the one this
	// process happens to have: a script that was refused the environment must
	// not reach it through a program it starts.
	source := env
	if source == nil {
		source = r.cfg.Env
	}
	cmd.Env = []string{}
	for k, v := range source {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if len(input) > 0 {
		cmd.Stdin = bytes.NewReader(input)
	}

	limit := r.cfg.MaxOutputBytes
	if limit == 0 {
		limit = 32 << 20
	}
	var out, errOut cappedBuffer
	out.limit, errOut.limit = limit, limit
	cmd.Stdout, cmd.Stderr = &out, &errOut

	if err := cmd.Start(); err != nil {
		return &result{err: err}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	if r.cfg.Timeout > 0 {
		select {
		case waitErr = <-done:
		case <-time.After(r.cfg.Timeout):
			cmd.Process.Kill()
			<-done
			return &result{
				stdout: out.Bytes(), stderr: errOut.Bytes(), code: -1,
				err: fmt.Errorf("%s took longer than %s", name, r.cfg.Timeout),
			}
		}
	} else {
		waitErr = <-done
	}

	res := &result{stdout: out.Bytes(), stderr: errOut.Bytes()}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			res.code = exitErr.ExitCode()
			res.err = fmt.Errorf("%s exited with %d", name, res.code)
		} else {
			res.code = -1
			res.err = waitErr
		}
	}
	return res
}

// execFileSync runs a program and returns what it wrote, failing if it did.
func (r *runner) execFileSync(name string, argv quickjs.Value, opts quickjs.Value, extra ...string) (string, error) {
	args := append(stringsOf(argv), extra...)
	res := r.start(name, args, optionString(opts, "cwd"), optionEnv(opts), optionInput(opts))
	if res.err != nil {
		return "", res.err
	}
	return string(res.stdout), nil
}

// spawnSync runs a program and reports how it went, whatever that was.
func (r *runner) spawnSync(name string, argv quickjs.Value, opts quickjs.Value) (quickjs.Value, error) {
	res := r.start(name, stringsOf(argv), optionString(opts, "cwd"), optionEnv(opts), optionInput(opts))
	return r.resultValue(res, opts)
}

// execFile runs a program and hands back a promise for its output.
func (r *runner) execFile(name string, argv quickjs.Value, opts quickjs.Value, extra ...string) *quickjs.Promise {
	p := r.rt.NewPromise()
	args := append(stringsOf(argv), extra...)
	dir, env, input := optionString(opts, "cwd"), optionEnv(opts), optionInput(opts)
	text := optionString(opts, "encoding") != "buffer"

	finish := func(res *result) {
		if res.err != nil {
			// The error carries what the program managed to say, which is
			// usually where the reason is.
			e := r.rt.NewError("Error", res.err.Error())
			e.Set("code", res.code)
			if text {
				e.Set("stdout", string(res.stdout))
				e.Set("stderr", string(res.stderr))
			} else {
				e.Set("stdout", r.rt.NewBytes(res.stdout))
				e.Set("stderr", r.rt.NewBytes(res.stderr))
			}
			p.Reject(e)
			return
		}
		out := r.rt.NewObject()
		if text {
			out.Set("stdout", string(res.stdout))
			out.Set("stderr", string(res.stderr))
		} else {
			out.Set("stdout", r.rt.NewBytes(res.stdout))
			out.Set("stderr", r.rt.NewBytes(res.stderr))
		}
		p.Resolve(out)
	}

	if r.cfg.Loop == nil {
		finish(r.start(name, args, dir, env, input))
		return p
	}
	loop := r.cfg.Loop
	loop.Begin()
	go func() {
		defer loop.Done()
		res := r.start(name, args, dir, env, input)
		loop.Post(func() { finish(res) })
	}()
	return p
}

// resultValue is what spawnSync hands back.
func (r *runner) resultValue(res *result, opts quickjs.Value) (quickjs.Value, error) {
	o := r.rt.NewObject()
	text := optionString(opts, "encoding") != "buffer"
	var stdout, stderr any = string(res.stdout), string(res.stderr)
	if !text {
		stdout, stderr = r.rt.NewBytes(res.stdout), r.rt.NewBytes(res.stderr)
	}
	err := errors.Join(
		o.Set("status", res.code),
		o.Set("stdout", stdout),
		o.Set("stderr", stderr),
	)
	if err != nil {
		return quickjs.Value{}, err
	}
	if res.err != nil {
		if err := o.Set("error", r.rt.NewError("Error", res.err.Error())); err != nil {
			return quickjs.Value{}, err
		}
	}
	return o, nil
}

// cappedBuffer collects output up to a limit and discards the rest, so that a
// program that writes for ever cannot exhaust the host's memory.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.limit - c.buf.Len()
	if room <= 0 {
		return len(p), nil
	}
	if len(p) > room {
		c.buf.Write(p[:room])
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

func (c *cappedBuffer) Bytes() []byte { return c.buf.Bytes() }

// stringsOf reads an array of strings from script.
func stringsOf(v quickjs.Value) []string {
	if !v.IsArray() {
		return nil
	}
	out := make([]string, 0, v.Len())
	for i := 0; i < v.Len(); i++ {
		el, err := v.Index(i)
		if err != nil {
			continue
		}
		out = append(out, el.String())
	}
	return out
}

func optionString(v quickjs.Value, name string) string {
	if v.Kind() != quickjs.KindObject {
		return ""
	}
	got, err := v.Get(name)
	if err != nil || got.Kind() != quickjs.KindString {
		return ""
	}
	return got.String()
}

func optionInput(v quickjs.Value) []byte {
	if v.Kind() != quickjs.KindObject {
		return nil
	}
	got, err := v.Get("input")
	if err != nil || got.IsNullish() {
		return nil
	}
	if b, ok := got.Bytes(); ok {
		return append([]byte(nil), b...)
	}
	return []byte(got.String())
}

// optionEnv reads an environment from script, or reports none.
func optionEnv(v quickjs.Value) map[string]string {
	if v.Kind() != quickjs.KindObject {
		return nil
	}
	got, err := v.Get("env")
	if err != nil || got.Kind() != quickjs.KindObject {
		return nil
	}
	out := map[string]string{}
	for _, k := range got.Keys() {
		val, err := got.Get(k)
		if err != nil {
			continue
		}
		out[k] = val.String()
	}
	return out
}
