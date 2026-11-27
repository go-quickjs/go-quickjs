//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

func init() {
	// The inline sites of a snapshot whose answers were asked on the
	// runtime's goroutine (jitWalkEager): the snapshot holds their effects,
	// which the feedback asked again gives no more (askedOf).
	jitSnapGatheredAt = append(jitSnapGatheredAt, func(s *jitSnapshot, m jitSnapMethod, _ int64) bool {
		return jitInlineMethods&(1<<m) != 0 && !s.d.inline.derives
	})
}
