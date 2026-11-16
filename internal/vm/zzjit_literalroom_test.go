//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"runtime"
	"testing"
	"unsafe"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

func TestJITLiteralArrayRoomWeakPools(t *testing.T) {
	pool := &abi.ObjectPool{Source: unsafe.Pointer(&jitPoolSource{array: true, room: 4096})}
	s := &jitState{arrayRooms: []weak.Pointer[abi.ObjectPool]{weak.Make(pool)}}
	pool = nil
	runtime.GC()
	jitLiteralArraySweep[struct{}](s)
	if len(s.arrayRooms) != 0 {
		t.Fatal("the sample list retained a discarded native pool")
	}
}
