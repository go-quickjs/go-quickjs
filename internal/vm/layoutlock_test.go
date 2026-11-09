package vm

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestLayoutLock fails where the layout lock has drifted: where a change
// moved an anchor of layoutlock.txt -- the hot code's, the Go runtime's hot
// data's -- away from its target, which go generate ./internal/vm puts it
// back to. It runs internal/cmd/layoutpad -check, which builds
// internal/cmd/v8bench plain and with the JIT as the targets were measured,
// some 20 seconds, under a few from the build cache. It is skipped under
// -short, and before it builds anything where the toolchain is not the
// one layoutlock.txt names or the platform has no targets there: the
// targets are one toolchain's layout on each platform.
func TestLayoutLock(t *testing.T) {
	if testing.Short() {
		t.Skip("builds internal/cmd/v8bench twice")
	}
	b, err := os.ReadFile("layoutlock.txt")
	if err != nil {
		t.Fatal(err)
	}
	var toolchain string
	platform := false
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) == 2 && f[0] == "toolchain":
			toolchain = f[1]
		case len(f) == 3 && f[0] == "config" && f[1] == runtime.GOOS+"/"+runtime.GOARCH:
			platform = true
		}
	}
	if toolchain != runtime.Version() {
		t.Skipf("the layout lock's targets are %s's layout, and this is %s", toolchain, runtime.Version())
	}
	if !platform {
		t.Skipf("the layout lock has no targets for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	gotool := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(gotool); err != nil {
		if gotool, err = exec.LookPath("go"); err != nil {
			t.Skip("no go command")
		}
	}
	// The tool's own builds run the go command the test does.
	env := append(os.Environ(), "PATH="+filepath.Dir(gotool)+string(os.PathListSeparator)+os.Getenv("PATH"))
	tool := filepath.Join(t.TempDir(), "layoutpad.exe")
	build := exec.Command(gotool, "build", "-o", tool, "../cmd/layoutpad")
	build.Env = env
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ../cmd/layoutpad: %v\n%s", err, out)
	}
	cmd := exec.Command(tool, "-check")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		t.Logf("%s", out)
	case errors.As(err, &exit) && exit.ExitCode() == 3:
		t.Skipf("%s", out)
	default:
		t.Fatalf("%v\n%s\nThe hot code or data has moved from the layout lock's targets: run go generate ./internal/vm "+
			"(go run ./internal/cmd/layoutpad), which pads it back, and commit the pads.", err, out)
	}
}
