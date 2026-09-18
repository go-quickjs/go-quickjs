// Command qjs runs JavaScript.
//
// It is the engine with a command line around it: a script or a module from a
// file, an expression from the command line, or a prompt to type at.
//
//	qjs script.js arg1 arg2      run a file
//	qjs -e 'console.log(1 + 1)'  run an expression
//	qjs                          read from a prompt
//	cat script.js | qjs -        run what arrives on standard input
//
// # What a script may do
//
// Nothing outside the process, unless it is allowed to. The engine has no
// ambient authority -- no filesystem, no network, no environment -- so the
// command line is where a capability is handed over:
//
//	qjs --allow-read=. script.js        read files under the current directory
//	qjs --allow-net=api.example.com ... reach one host
//	qjs -A script.js                    everything, for code you trust
//
// A script that tries to do something it was not allowed to gets an ordinary
// exception, which says which flag would have allowed it.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/stdlib"
)

// version is what --version reports. It is set from the build's own
// information when the binary was built from a module.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// options is the command line, parsed.
type options struct {
	eval        string
	hasEval     bool
	module      bool
	script      bool
	interactive bool
	check       bool
	file        string
	args        []string

	allowRead  []string
	allowWrite bool
	allowNet   []string
	allowEnv   bool
	allowRun   bool

	memoryLimit int64
	stackSize   int
	timeout     time.Duration
	noCodegen   bool
}

func run(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	opts, err := parseArgs(argv, stdout)
	if err != nil {
		fmt.Fprintln(stderr, "qjs:", err)
		fmt.Fprintln(stderr, "try 'qjs --help'")
		return 2
	}
	if opts == nil {
		// --help or --version, which have already been printed.
		return 0
	}

	rtOpts := []quickjs.Option{}
	if opts.memoryLimit > 0 {
		rtOpts = append(rtOpts, quickjs.WithMemoryLimit(opts.memoryLimit))
	}
	if opts.stackSize > 0 {
		rtOpts = append(rtOpts, quickjs.WithStackSize(opts.stackSize))
	}
	if opts.noCodegen {
		rtOpts = append(rtOpts, quickjs.WithoutCodeGeneration())
	}
	rt := quickjs.New(rtOpts...)
	defer rt.Close()

	loop := stdlib.NewLoop(rt)
	defer loop.Close()
	if err := install(rt, loop, opts, stdin, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, "qjs:", err)
		return 1
	}
	setModuleLoader(rt)

	// A rejection nothing took is a failure of the program, as it is in node:
	// it is reported where it happened and the exit code says so, rather than
	// disappearing.
	rejected := false
	rt.OnUnhandledRejection(func(reason quickjs.Value) {
		rejected = true
		fmt.Fprintln(stderr, "uncaught (in promise)", describe(rt, reason))
	})

	ctx := context.Background()
	if opts.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.timeout)
		defer cancel()
	}

	switch {
	case opts.check:
		return check(rt, opts, stderr)
	case opts.hasEval:
		if code := evaluate(rt, loop, ctx, opts, "<cmdline>", opts.eval, stdout, stderr); code != 0 {
			return code
		}
	case opts.file == "-":
		src, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintln(stderr, "qjs:", err)
			return 1
		}
		if code := evaluate(rt, loop, ctx, opts, "<stdin>", string(src), stdout, stderr); code != 0 {
			return code
		}
	case opts.file != "":
		src, err := os.ReadFile(opts.file)
		if err != nil {
			fmt.Fprintln(stderr, "qjs:", err)
			return 1
		}
		name, err := filepath.Abs(opts.file)
		if err != nil {
			name = opts.file
		}
		if code := evaluate(rt, loop, ctx, opts, name, string(src), stdout, stderr); code != 0 {
			return code
		}
	default:
		opts.interactive = true
	}

	if opts.interactive {
		return repl(rt, loop, ctx, stdin, stdout, stderr)
	}
	if rejected {
		return 1
	}
	return 0
}

// describe renders what a rejection carried: an Error shows its stack, and
// anything else is shown the way the console would show it.
func describe(rt *quickjs.Runtime, v quickjs.Value) string {
	if v.IsObject() {
		if stack, err := v.Get("stack"); err == nil && stack.Kind() == quickjs.KindString &&
			stack.String() != "" {
			return stack.String()
		}
	}
	return stdlib.Inspect(rt, v)
}

// install gives the runtime what the command line asked for.
func install(rt *quickjs.Runtime, loop *stdlib.Loop, opts *options, stdin io.Reader, stdout, stderr io.Writer) error {
	cfg := stdlib.Config{
		Stdout: stdout,
		Stderr: stderr,
		Loop:   loop,
		Process: &stdlib.Process{
			Args:    append([]string{"qjs", opts.file}, opts.args...),
			Cwd:     cwd(),
			Stdout:  stdout,
			Stderr:  stderr,
			Stdin:   stdin,
			Version: version,
			Exit:    func(code int) { os.Exit(code) },
		},
		OS: &stdlib.OSInfo{Real: true},
	}
	if opts.allowEnv {
		cfg.Process.Env = environment()
	}
	if len(opts.allowRead) > 0 || opts.allowWrite {
		root := ""
		if len(opts.allowRead) == 1 && opts.allowRead[0] != "" {
			root = opts.allowRead[0]
		}
		cfg.FS = &stdlib.FS{Root: root, ReadOnly: !opts.allowWrite, Loop: loop}
	}
	if len(opts.allowNet) > 0 {
		allowed := opts.allowNet
		cfg.Fetch = &stdlib.Fetch{
			Loop: loop,
			Allow: func(req *http.Request) error {
				if len(allowed) == 1 && allowed[0] == "" {
					return nil
				}
				host := req.URL.Hostname()
				for _, a := range allowed {
					if a == host || strings.HasSuffix(host, "."+a) {
						return nil
					}
				}
				return fmt.Errorf("%s is not allowed: pass --allow-net=%s", host, host)
			},
		}
	}
	if err := stdlib.Install(rt, cfg); err != nil {
		return err
	}
	// What was not allowed is explained rather than merely missing, so that a
	// script that tries says which flag it needed.
	return explainMissing(rt, opts)
}

// explainMissing installs stand-ins for the capabilities that were withheld.
func explainMissing(rt *quickjs.Runtime, opts *options) error {
	refuse := func(what, flag string) func() error {
		return func() error {
			return rt.Throw(rt.NewError("Error", fmt.Sprintf(
				"%s is not allowed: run qjs with %s", what, flag)))
		}
	}
	if len(opts.allowNet) == 0 {
		// fetch always hands back a promise, so a refusal is a rejection: code
		// that writes fetch(...).catch(...) sees what it expects.
		refused := func() *quickjs.Promise {
			p := rt.NewPromise()
			p.Reject(rt.NewError("Error",
				"network access is not allowed: run qjs with --allow-net"))
			return p
		}
		if err := rt.Set("fetch", refused); err != nil {
			return err
		}
	}
	if len(opts.allowRead) == 0 && !opts.allowWrite {
		denied := map[string]any{}
		for _, name := range []string{
			"readFileSync", "writeFileSync", "appendFileSync", "existsSync",
			"statSync", "readdirSync", "mkdirSync", "rmSync", "unlinkSync",
			"renameSync", "copyFileSync",
		} {
			denied[name] = refuse("filesystem access", "--allow-read")
		}
		denied["default"] = map[string]any{}
		for k, v := range denied {
			if k != "default" {
				denied["default"].(map[string]any)[k] = v
			}
		}
		if err := rt.SetModule("fs", denied); err != nil {
			return err
		}
		if err := rt.SetModule("node:fs", denied); err != nil {
			return err
		}
	}
	return nil
}

// evaluate runs source as a script or a module and then lets the loop finish.
func evaluate(rt *quickjs.Runtime, loop *stdlib.Loop, ctx context.Context,
	opts *options, name, src string, stdout, stderr io.Writer) int {
	var err error
	if opts.isModule(name, src) {
		_, err = rt.EvalModuleContext(ctx, name, src)
	} else {
		_, err = rt.EvalContext(ctx, src)
	}
	if err != nil {
		report(rt, stderr, err)
		return 1
	}
	if err := loop.Run(ctx); err != nil {
		report(rt, stderr, err)
		return 1
	}
	return 0
}

// isModule decides how to treat source that did not say.
//
// A file named .mjs is a module, one named .cjs is not, and anything else is
// judged by whether it uses the syntax only a module may: a file with an import
// or an export in it is a module, because it could not run as a script.
func (o *options) isModule(name, src string) bool {
	if o.module {
		return true
	}
	if o.script {
		return false
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mjs":
		return true
	case ".cjs":
		return false
	}
	return looksLikeModule(src)
}

// looksLikeModule reports whether source uses module syntax at the start of a
// line, which is where a declaration has to be.
func looksLikeModule(src string) bool {
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "import "), strings.HasPrefix(trimmed, "import{"),
			strings.HasPrefix(trimmed, "import*"), strings.HasPrefix(trimmed, "import\""),
			strings.HasPrefix(trimmed, "import'"):
			return true
		case strings.HasPrefix(trimmed, "export "), strings.HasPrefix(trimmed, "export{"),
			strings.HasPrefix(trimmed, "export*"), trimmed == "export default":
			return true
		}
	}
	return false
}

// check parses the input without running it.
func check(rt *quickjs.Runtime, opts *options, stderr io.Writer) int {
	src := opts.eval
	name := "<cmdline>"
	if opts.file != "" && opts.file != "-" {
		b, err := os.ReadFile(opts.file)
		if err != nil {
			fmt.Fprintln(stderr, "qjs:", err)
			return 1
		}
		src, name = string(b), opts.file
	}
	// Compiling without running is what a syntax check is: an early error is
	// raised by the compiler, and nothing else has a chance to happen.
	var err error
	if opts.isModule(name, src) {
		err = rt.CheckModuleSyntax(src)
	} else {
		err = rt.CheckSyntax(src)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// report prints what went wrong, with the stack when there is one.
func report(rt *quickjs.Runtime, w io.Writer, err error) {
	var jsErr *quickjs.Error
	if errors.As(err, &jsErr) {
		if stack := jsErr.Stack(); stack != "" {
			fmt.Fprintln(w, stack)
			return
		}
		fmt.Fprintln(w, jsErr.Error())
		return
	}
	fmt.Fprintln(w, "qjs:", err)
}

// ---------------------------------------------------------------------------
// The prompt
// ---------------------------------------------------------------------------

// repl reads lines and evaluates them.
//
// A line that does not parse yet is held on to rather than refused, so that a
// function or an object literal can be typed over several lines; an empty line
// abandons what is held.
func repl(rt *quickjs.Runtime, loop *stdlib.Loop, ctx context.Context,
	stdin io.Reader, stdout, stderr io.Writer) int {
	in := bufio.NewScanner(stdin)
	in.Buffer(make([]byte, 0, 64*1024), 16<<20)
	fmt.Fprintf(stdout, "qjs %s — type .help for the commands, Ctrl-D to leave\n", version)

	var held strings.Builder
	prompt := func() {
		if held.Len() > 0 {
			fmt.Fprint(stdout, "... ")
		} else {
			fmt.Fprint(stdout, "> ")
		}
	}
	prompt()
	for in.Scan() {
		line := in.Text()
		if held.Len() == 0 {
			switch strings.TrimSpace(line) {
			case ".exit":
				return 0
			case ".help":
				fmt.Fprintln(stdout, replHelp)
				prompt()
				continue
			case "":
				prompt()
				continue
			}
		}
		if held.Len() > 0 {
			held.WriteString("\n")
		}
		held.WriteString(line)
		src := held.String()

		// An input that is merely unfinished waits for more; one that is wrong
		// is reported at once.
		if err := rt.CheckSyntax(src); err != nil {
			if strings.TrimSpace(line) == "" {
				held.Reset()
				fmt.Fprintln(stderr, err)
			} else if isUnfinished(err) {
				prompt()
				continue
			} else {
				held.Reset()
				fmt.Fprintln(stderr, err)
			}
			prompt()
			continue
		}
		held.Reset()

		v, err := rt.EvalContext(ctx, src)
		if err != nil {
			report(rt, stderr, err)
			prompt()
			continue
		}
		if err := loop.Run(ctx); err != nil {
			report(rt, stderr, err)
			prompt()
			continue
		}
		// The last value is left in _, which is what a prompt is for.
		rt.Set("_", v)
		if !v.IsUndefined() {
			fmt.Fprintln(stdout, stdlib.Inspect(rt, v))
		}
		prompt()
	}
	fmt.Fprintln(stdout)
	return 0
}

// isUnfinished reports whether a syntax error is the kind more input would fix.
func isUnfinished(err error) bool {
	msg := err.Error()
	for _, sign := range []string{
		"unexpected end of input",
		"unexpected end of file",
		"unterminated",
		"expected \"}\"",
		"expected \")\"",
		"expected \"]\"",
	} {
		if strings.Contains(msg, sign) {
			return true
		}
	}
	return false
}

const replHelp = `  .exit    leave
  .help    this
  _        the value of the last expression
An unfinished line is continued: type the rest of it on the next line.`

// ---------------------------------------------------------------------------
// Modules
// ---------------------------------------------------------------------------

// setModuleLoader lets a module import another by path.
//
// A specifier that looks like a path is resolved against the module that named
// it, exactly as it would be on the web; a bare one is a module the host
// installed, and reaches the loader only when there is none, where it is an
// error that says so.
func setModuleLoader(rt *quickjs.Runtime) {
	rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
		if !strings.HasPrefix(specifier, ".") && !strings.HasPrefix(specifier, "/") &&
			!strings.HasPrefix(specifier, "file:") {
			return "", "", fmt.Errorf(
				"%q is not a module this runtime has; imports of files are written as paths", specifier)
		}
		path := strings.TrimPrefix(specifier, "file://")
		if !filepath.IsAbs(path) {
			base := filepath.Dir(referrer)
			if referrer == "" || referrer == "<cmdline>" || referrer == "<stdin>" {
				base = cwd()
			}
			path = filepath.Join(base, path)
		}
		resolved, err := filepath.Abs(path)
		if err != nil {
			return "", "", err
		}
		src, err := os.ReadFile(resolved)
		if err != nil {
			return "", "", err
		}
		return string(src), resolved, nil
	})
}

func cwd() string {
	dir, err := os.Getwd()
	if err != nil {
		return "/"
	}
	return dir
}

func environment() map[string]string {
	out := map[string]string{}
	for _, entry := range os.Environ() {
		if i := strings.IndexByte(entry, '='); i > 0 {
			out[entry[:i]] = entry[i+1:]
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// The command line
// ---------------------------------------------------------------------------

// parseArgs reads the command line. It returns nil options when there is
// nothing to run because something has already been printed.
func parseArgs(argv []string, stdout io.Writer) (*options, error) {
	opts := &options{}
	i := 0
	for ; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			i++
			break
		}
		if a == "-" || !strings.HasPrefix(a, "-") {
			break
		}
		name, value, hasValue := strings.Cut(a, "=")
		next := func(what string) (string, error) {
			if hasValue {
				return value, nil
			}
			if i+1 >= len(argv) {
				return "", fmt.Errorf("%s needs %s", name, what)
			}
			i++
			return argv[i], nil
		}
		switch name {
		case "-h", "--help":
			fmt.Fprintln(stdout, usage)
			return nil, nil
		case "-v", "--version":
			fmt.Fprintln(stdout, "qjs", buildVersion())
			return nil, nil
		case "-e", "--eval":
			code, err := next("code to run")
			if err != nil {
				return nil, err
			}
			opts.eval, opts.hasEval = code, true
		case "-m", "--module":
			opts.module = true
		case "-s", "--script":
			opts.script = true
		case "-i", "--interactive":
			opts.interactive = true
		case "--check":
			opts.check = true
		case "-A", "--allow-all":
			opts.allowRead = []string{""}
			opts.allowWrite = true
			opts.allowNet = []string{""}
			opts.allowEnv = true
			opts.allowRun = true
		case "--allow-read":
			dir := ""
			if hasValue {
				dir = value
			}
			opts.allowRead = append(opts.allowRead, dir)
		case "--allow-write":
			opts.allowWrite = true
			if len(opts.allowRead) == 0 {
				opts.allowRead = []string{""}
			}
			if hasValue && value != "" {
				opts.allowRead = []string{value}
			}
		case "--allow-net":
			hosts := ""
			if hasValue {
				hosts = value
			}
			if hosts == "" {
				opts.allowNet = append(opts.allowNet, "")
			} else {
				opts.allowNet = append(opts.allowNet, strings.Split(hosts, ",")...)
			}
		case "--allow-env":
			opts.allowEnv = true
		case "--memory-limit":
			v, err := next("a size in bytes")
			if err != nil {
				return nil, err
			}
			n, err := parseSize(v)
			if err != nil {
				return nil, err
			}
			opts.memoryLimit = n
		case "--stack-size":
			v, err := next("a number of slots")
			if err != nil {
				return nil, err
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, fmt.Errorf("--stack-size wants a number, not %q", v)
			}
			opts.stackSize = n
		case "--timeout":
			v, err := next("a duration")
			if err != nil {
				return nil, err
			}
			d, err := time.ParseDuration(v)
			if err != nil {
				return nil, fmt.Errorf("--timeout wants a duration such as 5s, not %q", v)
			}
			opts.timeout = d
		case "--no-code-generation":
			opts.noCodegen = true
		default:
			return nil, fmt.Errorf("unknown option %q", a)
		}
	}
	if i < len(argv) {
		opts.file = argv[i]
		opts.args = argv[i+1:]
	}
	if opts.hasEval && opts.file != "" {
		opts.args = append([]string{opts.file}, opts.args...)
		opts.file = ""
	}
	return opts, nil
}

// parseSize reads a byte count, which may carry a k, m or g.
func parseSize(s string) (int64, error) {
	mult := int64(1)
	trimmed := strings.TrimSpace(s)
	if len(trimmed) > 0 {
		switch trimmed[len(trimmed)-1] {
		case 'k', 'K':
			mult, trimmed = 1<<10, trimmed[:len(trimmed)-1]
		case 'm', 'M':
			mult, trimmed = 1<<20, trimmed[:len(trimmed)-1]
		case 'g', 'G':
			mult, trimmed = 1<<30, trimmed[:len(trimmed)-1]
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(trimmed), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("a size wants a number such as 64m, not %q", s)
	}
	return n * mult, nil
}

func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return version
}

const usage = `qjs runs JavaScript.

usage:
  qjs [options] [script.js] [arguments...]
  qjs [options] -e 'code'
  qjs [options]                 read from a prompt
  qjs [options] -               read the script from standard input

options:
  -e, --eval CODE         run CODE
  -m, --module            treat the input as a module
  -s, --script            treat the input as a script
  -i, --interactive       read from a prompt after running
      --check             check the syntax and run nothing
  -h, --help              this
  -v, --version           the version

what the script may do (nothing, unless said here):
  -A, --allow-all         everything below
      --allow-read[=DIR]  read files, confined to DIR when given
      --allow-write[=DIR] write them too
      --allow-net[=HOSTS] reach the network, or only these comma-separated hosts
      --allow-env         read the environment

bounds:
      --memory-limit N    stop the script at N bytes (64m, 1g)
      --stack-size N      value slots for all call frames
      --timeout D         stop after a duration such as 5s
      --no-code-generation   remove eval and the Function constructor`
