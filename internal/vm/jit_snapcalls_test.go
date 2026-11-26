//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import "slices"

// The calls' methods' scribbler (jitSnapScribblers): the closure's call
// caches emptied in place, as the interpreter writes them, and the realm's
// built-ins the methods compare with taken away.
func init() {
	jitSnapScribblers = append(jitSnapScribblers, func(r *Runtime, s *jitSnapshot) func() {
		var restores []func()
		if cl := s.rf.fb.cl; cl != nil {
			calls := cl.regCalls
			was := slices.Clone(calls)
			clear(calls)
			restores = append(restores, func() { copy(calls, was) })
		}
		realm := r.realmCallees()
		r.callFn, r.jitParseInt, r.jitBigToString = nil, nil, nil
		r.randomFn, r.powFn, r.atan2Fn, r.hypotFn = nil, nil, nil, nil
		r.jitCharCodeAt, r.jitFromCharCode = Undefined, Undefined
		r.jitSubstring, r.jitSlice, r.jitSubstr, r.jitCharAt = Undefined, Undefined, Undefined, Undefined
		r.jitSearches = [jitSearchKinds]*Object{}
		restores = append(restores, func() {
			r.callFn, r.jitParseInt, r.jitBigToString = realm.callFn, realm.parseInt, realm.bigToString
			r.randomFn, r.powFn, r.atan2Fn, r.hypotFn = realm.randomFn, realm.powFn, realm.atan2Fn, realm.hypotFn
			r.jitCharCodeAt, r.jitFromCharCode = realm.charCodeAt, realm.fromCharCode
			r.jitSubstring, r.jitSlice, r.jitSubstr, r.jitCharAt = realm.substring, realm.slice, realm.substr, realm.charAt
			r.jitSearches = realm.searches
		})
		return func() {
			for i := len(restores) - 1; i >= 0; i-- {
				restores[i]()
			}
		}
	})
}
