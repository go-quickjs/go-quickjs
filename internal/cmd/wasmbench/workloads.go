package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/go-quickjs/go-quickjs/internal/cmd/wasmbench/internal/worker"
	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

//go:embed testdata/wat/*.wat
var watFiles embed.FS

//go:embed testdata/goprog/*.go
var goprogFiles embed.FS

//go:embed node_worker.mjs
var nodeWorker []byte

// A workload is a module and what to do with it. Each run's result is
// checked, so that an engine that computes the wrong thing fails rather than
// wins.
type workload struct {
	suite, name string
	// file is a micro-benchmark's module in testdata/wat, whose export run
	// is called with arg.
	file string
	arg  uint32
	// args are goprog's arguments, for a program.
	args []string
	// want is run's i64 result as a signed decimal, or what the program
	// prints.
	want string
}

// The suites.
const (
	suiteMicro    = "micro"
	suitePrograms = "programs"
)

// workloads are the benchmarks, in the order they run. A micro-benchmark's
// expected result is the one Node computes; a program checks its own work,
// and its expected output is what the same source prints built for the host
// (TestProgramsNative).
var workloads = []workload{
	{suite: suiteMicro, name: "fib", file: "fib.wat", arg: 32, want: "2178309"},
	{suite: suiteMicro, name: "loop", file: "loop.wat", arg: 200_000, want: "214393550038"},
	{suite: suiteMicro, name: "i64", file: "i64.wat", arg: 100_000, want: "-7739113809381403063"},
	{suite: suiteMicro, name: "mandelbrot", file: "mandelbrot.wat", arg: 500, want: "5951648"},
	{suite: suiteMicro, name: "nbody", file: "nbody.wat", arg: 100_000, want: "-4628112272630223338"},
	{suite: suiteMicro, name: "bytes", file: "bytes.wat", arg: 10, want: "1306787712"},
	{suite: suiteMicro, name: "bulk", file: "bulk.wat", arg: 200, want: "4906722676528065937"},
	{suite: suiteMicro, name: "indirect", file: "indirect.wat", arg: 100_000, want: "3986621658"},
	{suite: suiteMicro, name: "brtable", file: "brtable.wat", arg: 100_000, want: "202015303213"},
	{suite: suiteMicro, name: "globals", file: "globals.wat", arg: 100_000, want: "4686070836355445248"},
	{suite: suiteMicro, name: "gc", file: "gc.wat", arg: 14, want: "-5805008426936700510"},
	{suite: suiteMicro, name: "simd", file: "simd.wat", arg: 1000, want: "2051588800606"},

	{suite: suitePrograms, name: "sha256", args: []string{"sha256", "8"},
		want: "sha256 8 MiB 117b42beb574a22fa98a2efa93f9351041498d2cc9da3d1fa65f468c222a680f\n"},
	{suite: suitePrograms, name: "sort", args: []string{"sort", "1000000"},
		want: "sort 1000000 min 230 max 2147483235 crc c499e925\n"},
	{suite: suitePrograms, name: "json", args: []string{"json", "200"},
		want: "json 200 bytes 344920 crc 0715a7c3\n"},
	{suite: suitePrograms, name: "regexp", args: []string{"regexp", "1000"},
		want: "regexp 1000 emails 1000 errors 493 api 186 crc 5eac257e\n"},
	{suite: suitePrograms, name: "gzip", args: []string{"gzip", "512"},
		want: "gzip 512 KiB compressed 127839 crc 0a501d01\n"},
	{suite: suitePrograms, name: "interp", args: []string{"interp", "200000"},
		want: "interp 200000 sum 599998 steps 4200009\n"},
}

// fullName is suite/name.
func (w workload) fullName() string { return w.suite + "/" + w.name }

// selected reports whether w matches one of the comma-separated prefixes
// in filter, by its name or its full name; an empty filter matches all.
func (w workload) selected(filter string) bool {
	if filter == "" {
		return true
	}
	for _, p := range strings.Split(filter, ",") {
		if p = strings.TrimSpace(p); p != "" && (strings.HasPrefix(w.name, p) || strings.HasPrefix(w.fullName(), p)) {
			return true
		}
	}
	return false
}

// request is the worker's request for w, its binary at bin.
func (w workload) request(engine, bin string) worker.Request {
	req := worker.Request{Engine: engine, Wasm: bin, Want: w.want, Warmup: *warmupFlag, Reps: *repsFlag}
	if w.file != "" {
		req.Kind, req.Export, req.Arg = "call", "run", w.arg
	} else {
		req.Kind, req.Args = "wasi", append([]string{"goprog"}, w.args...)
	}
	return req
}

// binary returns the path of w's module, made in the cache directory if it
// is not there: a micro-benchmark's text compiled by wasm/wat, or goprog
// built for wasip1.
func (w workload) binary() (string, error) {
	if w.file == "" {
		return goprogBinary()
	}
	src, err := watFiles.ReadFile("testdata/wat/" + w.file)
	if err != nil {
		return "", err
	}
	bin, err := wat.Compile(src)
	if err != nil {
		return "", fmt.Errorf("%s: %v", w.file, err)
	}
	return cacheFile(fmt.Sprintf("%s-%x.wasm", strings.TrimSuffix(w.file, ".wat"), hashOf(bin)), bin)
}

// cacheFile writes b to name in the cache directory unless a file of that
// name is there, and returns its path. Names carry a hash of what they hold.
func cacheFile(name string, b []byte) (string, error) {
	dir, err := cacheDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	tmp, err := os.CreateTemp(dir, name+".*")
	if err != nil {
		return "", err
	}
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), p)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return p, nil
}

// cacheDir is -cache, or wasmbench's directory in the user's cache.
func cacheDir() (string, error) {
	dir := *cacheFlag
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(base, "go-quickjs-wasmbench")
	}
	return dir, os.MkdirAll(dir, 0o755)
}

// goCommand is the go command: the one of the toolchain that built this
// program, or the one on PATH.
func goCommand() string {
	g := filepath.Join(runtime.GOROOT(), "bin", "go")
	if runtime.GOOS == "windows" {
		g += ".exe"
	}
	if _, err := os.Stat(g); err == nil {
		return g
	}
	return "go"
}

// goprogBinary builds testdata/goprog for GOOS=wasip1 GOARCH=wasm, once, and
// keeps it in the cache directory under the hash of its sources and the go
// command's version.
var goprogBinary = sync.OnceValues(func() (string, error) {
	goCmd := goCommand()
	ver, err := exec.Command(goCmd, "env", "GOVERSION").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOVERSION: %v", err)
	}
	files, err := fs.Glob(goprogFiles, "testdata/goprog/*.go")
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	h := sha256.New()
	fmt.Fprintf(h, "%s\n", strings.TrimSpace(string(ver)))
	srcs := map[string][]byte{"go.mod": []byte("module goprog\n\ngo 1.24\n")}
	for _, f := range files {
		b, err := goprogFiles.ReadFile(f)
		if err != nil {
			return "", err
		}
		srcs[path.Base(f)] = b
		fmt.Fprintf(h, "%s %d\n", path.Base(f), len(b))
		h.Write(b)
	}
	dir, err := cacheDir()
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("goprog-%s-%s.wasm", strings.TrimSpace(string(ver)), hex.EncodeToString(h.Sum(nil)[:6]))
	out := filepath.Join(dir, name)
	if _, err := os.Stat(out); err == nil {
		return out, nil
	}

	src, err := os.MkdirTemp("", "wasmbench-goprog")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(src)
	for name, b := range srcs {
		if err := os.WriteFile(filepath.Join(src, name), b, 0o644); err != nil {
			return "", err
		}
	}
	tmp := out + ".tmp"
	cmd := exec.Command(goCmd, "build", "-trimpath", "-o", tmp, ".")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0", "GOFLAGS=", "GOWORK=off")
	if msg, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("building goprog for wasip1: %v\n%s", err, msg)
	}
	if err := os.Rename(tmp, out); err != nil {
		return "", err
	}
	return out, nil
})

// hashOf is the first bytes of b's SHA-256, for a cached file's name.
func hashOf(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:6]
}
