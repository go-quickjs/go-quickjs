//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
)

// TestJITGoroutineTimes times the runtime goroutine's part of its compiles
// (TakeJITGoroutineTimes) by the clock it is given: on the spot, the
// compiles; in the background, each compile's preparation and its install,
// as many of each as compiles were handed over; and nothing again once
// taken.
func TestJITGoroutineTimes(t *testing.T) {
	prog := compileForTest(t, regBackgroundSrc)
	for _, background := range []bool{false, true} {
		t.Run(map[bool]string{false: "spot", true: "background"}[background], func(t *testing.T) {
			var ticks time.Duration
			jitTestSwitch(t, func(c *jitTestConfig) {
				c.compileClock = func() time.Duration {
					ticks += time.Microsecond
					return ticks
				}
			})
			r := newTestRuntime(t, Config{JIT: true})
			t.Cleanup(r.Close)
			r.SetJITWarmUp(1, 1)
			r.SetJITBackground(background)
			r.jitStress.background, r.jitStress.checks = background, 1
			if _, err := r.Run(prog); err != nil {
				t.Fatal(err)
			}
			fn, _ := r.GetProp(Obj(r.Global()), r.Intern("work"))
			for range 6 {
				if _, err := r.Call(fn, Undefined, []Value{Float(2000)}); err != nil {
					t.Fatal(err)
				}
			}
			got := r.TakeJITGoroutineTimes()
			if background {
				if got.Prepares == 0 || uint64(got.Prepares) != r.jit.bgSubmitted || got.Installs != got.Prepares {
					t.Fatalf("in the background: %+v, %d handed over", got, r.jit.bgSubmitted)
				}
				if got.Prepared < time.Duration(got.Prepares)*time.Microsecond || got.Installed < time.Duration(got.Installs)*time.Microsecond {
					t.Fatalf("in the background, untimed: %+v", got)
				}
			} else if got.Spots == 0 || got.Spot < time.Duration(got.Spots)*time.Microsecond || got.Prepares != 0 || got.Installs != 0 {
				t.Fatalf("on the spot: %+v", got)
			}
			if again := r.TakeJITGoroutineTimes(); again != (JITGoroutineTimes{}) {
				t.Fatalf("taken again: %+v", again)
			}
		})
	}
}

// The goroutine's times leave out the host's hooks a compile calls (the
// tier trace's, -jitcode's), which are the host's: each hook here takes an
// hour by the test's clock, and no compile's time has one.
func TestJITGoroutineTimesLeaveOutHooks(t *testing.T) {
	prog := compileForTest(t, regBackgroundSrc)
	for _, background := range []bool{false, true} {
		t.Run(map[bool]string{false: "spot", true: "background"}[background], func(t *testing.T) {
			var ticks time.Duration
			tiers, codes := 0, 0
			jitTestSwitch(t, func(c *jitTestConfig) {
				c.compileClock = func() time.Duration {
					ticks += time.Microsecond
					return ticks
				}
				c.tierHook = func(JITTierEvent) {
					tiers++
					ticks += time.Hour
				}
				c.codeHook = func(JITFunction, []byte, *mir.Code) {
					codes++
					ticks += time.Hour
				}
			})
			r := newTestRuntime(t, Config{JIT: true})
			t.Cleanup(r.Close)
			r.SetJITWarmUp(1, 1)
			r.SetJITBackground(background)
			r.jitStress.background, r.jitStress.checks = background, 1
			if _, err := r.Run(prog); err != nil {
				t.Fatal(err)
			}
			fn, _ := r.GetProp(Obj(r.Global()), r.Intern("work"))
			for range 6 {
				if _, err := r.Call(fn, Undefined, []Value{Float(2000)}); err != nil {
					t.Fatal(err)
				}
			}
			got := r.TakeJITGoroutineTimes()
			if tiers == 0 || codes == 0 || got.Spots+got.Installs == 0 {
				t.Fatalf("%d tier events, %d code records, times %+v: want compiles and hooks called", tiers, codes, got)
			}
			if got.Spot >= time.Hour || got.Prepared >= time.Hour || got.Installed >= time.Hour {
				t.Fatalf("times %+v: want the hooks' hours left out", got)
			}
		})
	}
}
