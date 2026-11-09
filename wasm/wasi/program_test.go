package wasi_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-quickjs/go-quickjs/wasm"
	"github.com/go-quickjs/go-quickjs/wasm/sys"
	"github.com/go-quickjs/go-quickjs/wasm/wasi"
)

var (
	progOnce  sync.Once
	progWasm  []byte
	progErr   error
	progCache = wasm.NewCompilationCache()
)

// prog returns testdata/prog built for GOOS=wasip1, skipping the test
// under -short or where the go command cannot build it.
func prog(t *testing.T) []byte {
	t.Helper()
	if testing.Short() {
		t.Skip("builds a GOOS=wasip1 program")
	}
	progOnce.Do(func() {
		goCmd := filepath.Join(runtime.GOROOT(), "bin", "go")
		if _, err := os.Stat(goCmd); err != nil {
			if goCmd, err = exec.LookPath("go"); err != nil {
				progErr = err
				return
			}
		}
		dir, err := os.MkdirTemp("", "wasi-prog")
		if err != nil {
			progErr = err
			return
		}
		defer os.RemoveAll(dir)
		out := filepath.Join(dir, "prog.wasm")
		cmd := exec.Command(goCmd, "build", "-o", out, ".")
		cmd.Dir = filepath.Join("testdata", "prog")
		cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0", "GOFLAGS=")
		if msg, err := cmd.CombinedOutput(); err != nil {
			progErr = errors.New(string(msg))
			return
		}
		progWasm, progErr = os.ReadFile(out)
	})
	if progErr != nil {
		t.Skipf("cannot build a wasip1 program: %v", progErr)
	}
	return progWasm
}

// result is what a run of prog printed and returned.
type result struct {
	stdout, stderr string
	err            error
}

// runProg runs prog with args under cfg, its streams captured.
func runProg(t *testing.T, cfg wasm.ModuleConfig, args ...string) result {
	t.Helper()
	bin := prog(t)
	ctx := context.Background()
	r := wasm.NewRuntimeWithConfig(ctx, wasm.NewRuntimeConfig().WithCompilationCache(progCache))
	defer r.Close(ctx)
	wasi.MustInstantiate(ctx, r)
	compiled, err := r.CompileModule(ctx, bin)
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	cfg = cfg.WithArgs(append([]string{"prog"}, args...)...).WithStdout(&out).WithStderr(&errb)
	_, err = r.InstantiateModule(ctx, compiled, cfg)
	return result{out.String(), errb.String(), err}
}

// mustRun runs prog and fails the test unless it exits with code zero.
func mustRun(t *testing.T, cfg wasm.ModuleConfig, args ...string) string {
	t.Helper()
	res := runProg(t, cfg, args...)
	if res.err != nil {
		t.Fatalf("%v: %v\nstdout:\n%s\nstderr:\n%s", args, res.err, res.stdout, res.stderr)
	}
	return res.stdout
}

func TestProgramArgsEnv(t *testing.T) {
	cfg := wasm.NewModuleConfig().WithEnv("HOME", "/home/gopher").WithEnv("A", "1")
	got := mustRun(t, cfg, "args", "one", "two words", "")
	want := "arg one\narg two words\narg \nenv A=1\nenv HOME=/home/gopher\nHOME /home/gopher\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestProgramStdio(t *testing.T) {
	res := runProg(t, wasm.NewModuleConfig(), "stdio")
	if res.err != nil || res.stdout != "to stdout\n" || res.stderr != "to stderr\n" {
		t.Errorf("got %+v", res)
	}
}

func TestProgramExit(t *testing.T) {
	res := runProg(t, wasm.NewModuleConfig(), "exit", "3")
	var exit *sys.ExitError
	if !errors.As(res.err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("got %v, want exit code 3", res.err)
	}
	if res.stdout != "exiting\n" {
		t.Errorf("stdout %q", res.stdout)
	}
	if res := runProg(t, wasm.NewModuleConfig(), "exit", "0"); res.err != nil {
		t.Errorf("exit 0: %v", res.err)
	}
}

func TestProgramStdin(t *testing.T) {
	cfg := wasm.NewModuleConfig().WithStdin(strings.NewReader("some input"))
	if got, want := mustRun(t, cfg, "stdin"), "read 10: SOME INPUT\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := mustRun(t, wasm.NewModuleConfig(), "stdin"), "read 0: \n"; got != want {
		t.Errorf("no stdin: got %q, want %q", got, want)
	}
}

func TestProgramFiles(t *testing.T) {
	dir := t.TempDir()
	cfg := wasm.NewModuleConfig().WithFSConfig(wasm.NewFSConfig().WithDirMount(dir, "/work"))
	got := mustRun(t, cfg, "files", "/work")
	want := "readdir b.txt:false deep:true\n" +
		"rmdir non-empty: Directory not empty\n" +
		"stat removed: No such file or directory\n" +
		"open missing: No such file or directory\n" +
		"excl: File exists\n" +
		"left a.txt\n" +
		"files ok\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	b, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil || string(b) != "Jello+1" {
		t.Errorf("host sees %q, %v", b, err)
	}
	// The same at the root, with a relative path.
	dir2 := t.TempDir()
	cfg = wasm.NewModuleConfig().WithFSConfig(wasm.NewFSConfig().WithDirMount(dir2, "/"))
	if got := mustRun(t, cfg, "files", "."); got != want {
		t.Errorf("at the root: got\n%s\nwant\n%s", got, want)
	}
}

func TestProgramReadOnly(t *testing.T) {
	fsys := fstest.MapFS{
		"hello.txt":     {Data: []byte("hello there")},
		"sub/inner.txt": {Data: []byte("inside")},
	}
	want := "hello.txt: \"hello there\"\n" +
		"entry hello.txt false\n" +
		"entry sub true\n" +
		"inner: \"inside\"\n" +
		"size 11\n" +
		"after seek: \"there\"\n" +
		"write: Read-only file system\n" +
		"mkdir: Read-only file system\n" +
		"remove: Read-only file system\n"
	if got := mustRun(t, wasm.NewModuleConfig().WithFS(fsys), "readonly", "/"); got != want {
		t.Errorf("fs.FS: got\n%s\nwant\n%s", got, want)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello there"), 0o644)
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "inner.txt"), []byte("inside"), 0o644)
	cfg := wasm.NewModuleConfig().WithFSConfig(wasm.NewFSConfig().WithReadOnlyDirMount(dir, "/ro"))
	if got := mustRun(t, cfg, "readonly", "/ro"); got != want {
		t.Errorf("read-only directory: got\n%s\nwant\n%s", got, want)
	}
}

func TestProgramSandbox(t *testing.T) {
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o644)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "inside"), []byte("in"), 0o644)
	links := true
	rel, err := filepath.Rel(dir, outside)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "out")); err != nil {
		links = false
		t.Logf("no symbolic links: %v", err)
	} else if err := os.Symlink(rel, filepath.Join(dir, "relout")); err != nil {
		t.Fatal(err)
	}
	cfg := wasm.NewModuleConfig().WithFSConfig(wasm.NewFSConfig().WithDirMount(dir, "/data"))
	// The program resolves ".." itself: "/data/../secret" is "/secret",
	// which no mount holds.
	got := mustRun(t, cfg, "open", "/data/inside", "/data/../data/inside", "/data/../secret", "/secret")
	want := "/data/inside: ok\n" +
		"/data/../data/inside: ok\n" +
		"/data/../secret: Bad file number\n" +
		"/secret: Bad file number\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if links {
		got := mustRun(t, cfg, "open", "/data/out/secret", "/data/relout/secret")
		want := "/data/out/secret: Operation not permitted\n" +
			"/data/relout/secret: Operation not permitted\n"
		if got != want {
			t.Errorf("through a link: got %q, want %q", got, want)
		}
	}
}

func TestProgramClock(t *testing.T) {
	cfg := wasm.NewModuleConfig().WithSysWalltime().WithSysNanotime().WithSysNanosleep()
	start := time.Now()
	got := mustRun(t, cfg, "clock")
	if want := "year true\nslept true\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Errorf("the program did not sleep")
	}
}

func TestProgramRandom(t *testing.T) {
	a := mustRun(t, wasm.NewModuleConfig(), "rand")
	b := mustRun(t, wasm.NewModuleConfig(), "rand")
	if a != b || len(a) != 65 {
		t.Errorf("the default source is not deterministic: %q, %q", a, b)
	}
	cfg := wasm.NewModuleConfig().WithRandSource(bytes.NewReader(bytes.Repeat([]byte{0xab}, 64)))
	if got := mustRun(t, cfg, "rand"); got != strings.Repeat("ab", 32)+"\n" {
		t.Errorf("got %q", got)
	}
}

func TestProgramGoroutines(t *testing.T) {
	cfg := wasm.NewModuleConfig().WithSysWalltime().WithSysNanotime().WithSysNanosleep()
	got := mustRun(t, cfg, "goroutines")
	if want := "order 1 2 3 4 5\nticked true\nelapsed true\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// The fake clocks: the program sees time pass as it reads the clock.
	if got := mustRun(t, wasm.NewModuleConfig(), "goroutines"); !strings.HasPrefix(got, "order 1 2 3 4 5\n") {
		t.Errorf("fake clocks: got %q", got)
	}
}
