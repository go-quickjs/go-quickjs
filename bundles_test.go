package quickjs_test

import (
	"testing"

	"github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/bundletest"
)

func TestOpenSourceBundles(t *testing.T) {
	for _, bundle := range bundletest.All {
		t.Run(bundle.Name, func(t *testing.T) {
			src := bundletest.Read(t, bundle)
			rt := quickjs.New()
			defer rt.Close()
			if _, err := rt.Eval(bundle.Prelude); err != nil {
				t.Fatal(err)
			}
			if _, err := rt.EvalFile(bundle.File, src); err != nil {
				t.Fatal(err)
			}
			v, err := rt.Eval(bundle.Check)
			if err != nil || v.String() != bundle.Want {
				t.Fatalf("got %v, %v; want %s", v, err, bundle.Want)
			}
		})
	}
}

// BenchmarkLoadBundle parses, compiles and executes in a new runtime each
// time. Runtime construction, host setup, checks and Close are not timed.
// Use -benchtime=1x in separate processes to measure a fresh-process load.
func BenchmarkLoadBundle(b *testing.B) {
	for _, bundle := range bundletest.All {
		b.Run(bundle.Name, func(b *testing.B) {
			src := bundletest.Read(b, bundle)
			b.ReportAllocs()
			b.SetBytes(int64(len(src)))
			b.ResetTimer()
			b.StopTimer()
			for i := 0; i < b.N; i++ {
				rt := quickjs.New()
				if _, err := rt.Eval(bundle.Prelude); err != nil {
					rt.Close()
					b.Fatal(err)
				}
				b.StartTimer()
				_, err := rt.EvalFile(bundle.File, src)
				b.StopTimer()
				if err != nil {
					rt.Close()
					b.Fatal(err)
				}
				v, err := rt.Eval(bundle.Check)
				if err != nil || v.String() != bundle.Want {
					rt.Close()
					b.Fatalf("got %v, %v; want %s", v, err, bundle.Want)
				}
				rt.Close()
			}
		})
	}
}
