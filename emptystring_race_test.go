package quickjs_test

import (
	"sync"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestEmptyStringIsNotWritten pins that runtimes on separate goroutines
// share the empty string without writing to it: matching against it used to
// cache its code units in it, which the race detector reports (KI-12).
func TestEmptyStringIsNotWritten(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rt := quickjs.New()
			defer rt.Close()
			if _, err := rt.Eval(`for (let i = 0; i < 100; i++) { "".replace("", "$'"); /x*/.exec(""); "".split("") }`); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}
