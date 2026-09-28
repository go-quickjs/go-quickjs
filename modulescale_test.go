package quickjs_test

import (
	"fmt"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestModuleChainLinksInLinearTime pins that linking a chain of modules takes
// time in proportion to its length, and asks the loader once for each import:
// every module linked loaded the rest of the chain's graph again, and every
// lookup of an import asked the loader again, so a chain of 8000 took
// seconds (KI-16).
func TestModuleChainLinksInLinearTime(t *testing.T) {
	const n = 8000
	rt := quickjs.New()
	defer rt.Close()
	calls := 0
	rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
		calls++
		var i int
		fmt.Sscanf(specifier, "./m%d.js", &i)
		if i == n {
			return "export const depth = 0;", specifier, nil
		}
		return fmt.Sprintf(`import { depth as d } from "./m%d.js"; export const depth = d + 1;`, i+1), specifier, nil
	})
	start := time.Now()
	ns, err := rt.EvalModule("main.js", `import { depth } from "./m1.js"; export const total = depth;`)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := ns.Get("total"); v.Int() != n-1 {
		t.Errorf("total = %v", v)
	}
	if calls != n {
		t.Errorf("the loader was asked %d times for %d modules", calls, n)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v", d)
	}
}
