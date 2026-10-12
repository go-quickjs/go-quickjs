//go:build quickjs_jit && !android && !ios && arm64

package jit

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDarwinHardenedRuntimeRefused(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	hardened := filepath.Join(t.TempDir(), "hardened.test")
	if err := os.WriteFile(hardened, contents, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("codesign", "--force", "--sign", "-", "--options", "runtime", "--timestamp=none", hardened)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sign hardened test: %v\n%s", err, out)
	}
	cmd = exec.Command(hardened, "-test.run=^TestExecutableMemoryPolicy$", "-test.v")
	cmd.Env = append(os.Environ(), "QUICKJS_EXPECT_JIT_DENIED=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hardened executable must refuse safely: %v\n%s", err, out)
	} else {
		t.Logf("%s", out)
	}
}
