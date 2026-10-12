package jit

import (
	"os"
	"runtime"
	"testing"
)

func TestRequiredBackend(t *testing.T) {
	if os.Getenv("QUICKJS_REQUIRE_JIT") != "" && !Supported() {
		t.Fatalf("native backend required but excluded on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
}
