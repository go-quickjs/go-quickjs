package vm

import (
	"bytes"
	"os"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/vm/internal/snapgen"
)

// The snapshot's answers and askers are jitRegFeedback's methods, all of
// them (snapgen): a method added to the feedback and not to them would have
// the snapshot miss it, or answer it wrongly.
func TestSnapshotGenerated(t *testing.T) {
	t.Parallel()
	snapshot, err := snapgen.Generate(".")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("zzjit_snapshot_gen.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), snapshot) {
		t.Error("zzjit_snapshot_gen.go is out of date: run go generate ./internal/vm")
	}
}
