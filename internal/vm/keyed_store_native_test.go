//go:build amd64 || arm64

package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// TestRegNativeKeyedStores runs keyedStoreCases by the native handlers
// against the Go table, marking and not: the native keyed stores (the
// nativegen's keyedStore) give the same answers and exit as often either
// way. Then a loop storing four keys by a literal's computed key and by an
// assignment, into a new object each turn, which once the sites' caches
// hold the four pairs exits at neither operation.
func TestRegNativeKeyedStores(t *testing.T) {
	for _, c := range keyedStoreCases {
		t.Run(c.name, func(t *testing.T) {
			nativeCheck(t, c.src, "")
			if got := nativeRunSrc(t, c.src, true); got != c.want {
				t.Errorf("native: got\n%s\nwant\n%s", got, c.want)
			}
		})
	}
	nativeCheck(t, keyedStoreCases[0].src, `
		var keys = ['ka', 'kb', 'kc', 'kd'], s = 0
		for (var j = 0; j < 1000; j++) {
			var o = { [keys[j & 3]]: j, v: 1 }
			var p = { x: 1 }
			p[keys[j & 3]] = j
			p[keys[j & 3]] = j + 1
			s += o.v + p[keys[j & 3]]
		}
		String(s)`, bytecode.RSetIndex, bytecode.RDefineIndex)
}
