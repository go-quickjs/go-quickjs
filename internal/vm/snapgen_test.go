package vm

import (
	"bytes"
	"os"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/vm/internal/snapgen"
)

// The snapshot's answers and askers, and the proxy noting what a build
// asks, are jitFeedback's methods, all of them (snapgen): a method added to
// the feedback and not to them would have the snapshot miss it, or answer
// it wrongly.
func TestSnapshotGenerated(t *testing.T) {
	snapshot, asked, err := snapgen.Generate(".")
	if err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string][]byte{"zzjit_snapshot_gen.go": snapshot, "zzjit_asked_gen_test.go": asked} {
		got, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
			t.Errorf("%s is out of date: run go generate ./internal/vm", file)
		}
	}
}
