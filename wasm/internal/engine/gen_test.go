package engine

import (
	"bytes"
	"os"
	osexec "os/exec"
	"path/filepath"
	"testing"
)

// TestNumericGenerated fails while ops_num_gen.go or reg_num_gen.go differs
// from what internal/numgen writes; go generate ./wasm/internal/engine
// rewrites them.
func TestNumericGenerated(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the generator")
	}
	dir := t.TempDir()
	files := []string{"ops_num_gen.go", "reg_num_gen.go"}
	args := []string{"run", "./internal/numgen"}
	for _, f := range files {
		args = append(args, filepath.Join(dir, f))
	}
	if b, err := osexec.Command("go", args...).CombinedOutput(); err != nil {
		t.Fatalf("numgen: %v\n%s", err, b)
	}
	for _, f := range files {
		want, err := os.ReadFile(filepath.Join(dir, f))
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
