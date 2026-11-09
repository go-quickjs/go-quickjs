package engine

import (
	"bytes"
	"os"
	osexec "os/exec"
	"path/filepath"
	"testing"
)

// TestNumericGenerated fails while ops_num_gen.go differs from what
// internal/numgen writes; go generate ./wasm/internal/engine rewrites it.
func TestNumericGenerated(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the generator")
	}
	out := filepath.Join(t.TempDir(), "ops_num_gen.go")
	cmd := osexec.Command("go", "run", "./internal/numgen", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("numgen: %v\n%s", err, b)
	}
	want, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("ops_num_gen.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("ops_num_gen.go is out of date: run go generate ./wasm/internal/engine")
	}
}
