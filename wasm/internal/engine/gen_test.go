package engine

import (
	"bytes"
	"os"
	osexec "os/exec"
	"path/filepath"
	"testing"
)

// TestNumericGenerated fails while reg_num_gen.go or regdef/num_gen.go differs
// from what internal/numgen writes; go generate ./wasm/internal/engine
// rewrites them.
func TestNumericGenerated(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the generator")
	}
	checkGenerated(t, "./internal/numgen", "reg_num_gen.go", "internal/regdef/num_gen.go")
}

// checkGenerated runs generator gen, which writes files, into a temporary
// directory, and fails where a file differs from the package's.
func checkGenerated(t *testing.T, gen string, files ...string) {
	t.Helper()
	dir := t.TempDir()
	args := []string{"run", gen}
	for _, f := range files {
		args = append(args, filepath.Join(dir, filepath.Base(f)))
	}
	if b, err := osexec.Command("go", args...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", gen, err, b)
	}
	for _, f := range files {
		want, err := os.ReadFile(filepath.Join(dir, filepath.Base(f)))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
			t.Fatalf("%s is out of date: run go generate ./wasm/internal/engine", f)
		}
	}
}
