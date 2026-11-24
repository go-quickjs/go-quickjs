//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"os"
	"sync"
	"sync/atomic"

	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
)

// The calls the optimizing tier inlines (wasm/internal/jit/opt, inline.go)
// make no frames, and what their bodies report -- a trap's offset, an
// exit's, a return stack entry's -- is an offset past the module's bytes
// that the Code gives them: an inlined site, which says the frames the
// calls would have made. Go reads them back wherever it makes the call
// stack of native frames (jitSync), so that a trap's frames, a caller and
// the call depth are those of the calls.

// jitInline turns inlining off where WASM_JIT_INLINE is "off", for tests
// and measurements.
var jitInline = os.Getenv("WASM_JIT_INLINE") != "off"

// inlineSite is an instruction of an inlined body: the frames the calls
// around it would have made, the outermost first, and its offset in the
// innermost.
type inlineSite struct {
	frames []wjit.InlineFrame
	at     uint32
}

// inlineSites are a Code's inlined sites, each once: the offsets
// past base, in the order they were given, which a compile in the
// background may add to while native code runs.
type inlineSites struct {
	mu    sync.Mutex
	base  uint32
	ids   map[string]uint32
	sites atomic.Pointer[[]inlineSite]
}

// maxInlineSite bounds the offsets the sites take: native code holds them
// in 32-bit immediates.
const maxInlineSite = 1<<31 - 1

// site returns the offset of the inlined site of frames and at, giving it
// one if it has none.
func (s *inlineSites) site(frames []wjit.InlineFrame, at uint32) (uint32, bool) {
	var buf [8*8 + 4]byte
	key := buf[:0]
	for _, f := range frames {
		key = appendU32(appendU32(key, f.Fn), f.Call)
	}
	key = appendU32(key, at)
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.ids[string(key)]; ok {
		return s.base + id, true
	}
	var list []inlineSite
	if p := s.sites.Load(); p != nil {
		list = *p
	}
	id := uint32(len(list))
	if uint64(s.base)+uint64(id) >= maxInlineSite {
		return 0, false
	}
	if s.ids == nil {
		s.ids = map[string]uint32{}
	}
	s.ids[string(key)] = id
	// Readers hold the slice they loaded, whose elements this append
	// leaves as they are.
	list = append(list, inlineSite{frames: append([]wjit.InlineFrame(nil), frames...), at: at})
	s.sites.Store(&list)
	return s.base + id, true
}

func appendU32(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

// get returns the inlined site at offset off, or nil where off is an
// instruction's.
func (s *inlineSites) get(off uint32) *inlineSite {
	if off < s.base {
		return nil
	}
	p := s.sites.Load()
	if p == nil || int(off-s.base) >= len(*p) {
		return nil
	}
	return &(*p)[off-s.base]
}

// any reports whether the Code has inlined sites: its native code may have
// return stack entries with unwritten ones below them.
func (s *inlineSites) any() bool { return s.sites.Load() != nil }

// depth is how many inlined calls deep the instruction at offset off is.
func (s *inlineSites) depth(off uint32) int {
	if site := s.get(off); site != nil {
		return len(site.frames)
	}
	return 0
}

// leaf is the offset in its function of the instruction at offset off: an
// inlined site's in the innermost callee.
func (s *inlineSites) leaf(off uint32) uint32 {
	if site := s.get(off); site != nil {
		return site.at
	}
	return off
}

// outer is the offset of the instruction at offset off in the function
// whose code it is: for an inlined site, that of the outermost call.
func (s *inlineSites) outer(off uint32) uint32 {
	if site := s.get(off); site != nil {
		return site.frames[0].Call
	}
	return off
}

// rsEntries returns the indices in the return stack of the level's
// entries, the first first: each entry k entries past the one below it
// where the call was made k inlined calls deep (wjit.Site). It reuses buf.
func (x *exec) rsEntries(lv *jitLevel, buf []int) []int {
	rs := x.jit.rs
	first, last := x.rsIndex(lv.rsBase), x.rsIndex(lv.ctx.RSP)
	buf = buf[:0]
	if !lv.cj.inline.any() {
		for i := first; i < last; i += wjit.RSEntryWords {
			buf = append(buf, i)
		}
		return buf
	}
	for i := last - wjit.RSEntryWords; i >= first; {
		buf = append(buf, i)
		i -= wjit.RSEntryWords * (1 + lv.cj.inline.depth(uint32(rs[i+2])))
	}
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return buf
}

// syncAt sets the innermost frame's offset to off and, for an inlined
// site, adds the frames its calls would have made.
func (x *exec) syncAt(lv *jitLevel, off uint32) {
	site := lv.cj.inline.get(off)
	if site == nil {
		x.calls[len(x.calls)-1].offset = off
		return
	}
	x.calls[len(x.calls)-1].offset = site.frames[0].Call
	for i, f := range site.frames {
		at := site.at
		if i+1 < len(site.frames) {
			at = site.frames[i+1].Call
		}
		x.calls = append(x.calls, callSite{fn: lv.inst.Funcs[f.Fn], offset: at})
	}
}
