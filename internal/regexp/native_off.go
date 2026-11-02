//go:build !quickjs_jit

package regexp

// A build without the JIT matches every pattern with the matchers: see
// native_jit.go for what one with it does.

const nativeBuilt = false

type nativeSlot struct{}

type nativeRun struct{}

func (r *nativeRun) large() bool { return false }

func (re *Regexp) nativeTick(bool) {}

// attempt is the match of the pattern at pos.
func (m *matcher) attempt(re *Regexp, pos int) (bool, error) { return m.run(re.prog.code, pos) }

// attempt is the match of the pattern at pos.
func (m *matcherASCII) attempt(re *Regexp, pos int) (bool, error) { return m.run(re.prog.code, pos) }

// attempt is the match of the pattern at pos.
func (m *matcherUTF8) attempt(re *Regexp, pos int) (bool, error) { return m.run(re.prog.code, pos) }

// searchNative is search by native code, which there is none of.
func (m *matcher) searchNative(*Regexp, []int, int, bool, *requiredLit) ([]int, bool, error) {
	return nil, false, nil
}

// searchNative is search by native code, which there is none of.
func (m *matcherASCII) searchNative(*Regexp, []int, int, bool, *requiredLit) ([]int, bool, error) {
	return nil, false, nil
}

// searchNative is search by native code, which there is none of.
func (m *matcherUTF8) searchNative(*Regexp, []int, int, bool, *requiredLit) ([]int, bool, error) {
	return nil, false, nil
}
