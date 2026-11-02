//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/regexp"
)

// jitRegExpNative has a pattern the runtime compiled matched by native code
// once it has been matched often enough (regexp.Regexp.UseNative): code
// placed in the runtime's arena and counted against its JIT's budget, as
// V8's regular expressions' code is in its code space. Under
// QJS_JIT_STRESS's threshold a pattern's first match makes it.
func (r *Runtime) jitRegExpNative(re *regexp.Regexp) {
	s := r.jitState()
	if s.unavailable {
		return
	}
	if s.regexpHost == nil {
		arena := s.arena
		h := &regexp.NativeHost{
			Place: func(code []byte) (regexp.NativeCode, error) {
				c, err := jit.PlaceRaw(arena, code)
				if err != nil {
					return nil, err
				}
				return c, nil
			},
			// The budget, and what is old, as a function's compile has them
			// before it asks for room.
			Room: func(n int) bool {
				s.budget = r.jitBudget()
				s.age(s.epoch())
				return s.makeRoom(n)
			},
		}
		if r.jitStress.threshold {
			h.After = 1
		}
		s.regexpHost = h
	}
	re.UseNative(s.regexpHost)
}
