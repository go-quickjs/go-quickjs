package decoder

import "testing"

// bigModule is a module of n functions, copies of the seeds' bodies.
func bigModule(n int) []byte {
	numeric := numericSeed()
	control := controlSeed()
	_, nb, _ := codeBodies(numeric)
	_, cb, _ := codeBodies(control)
	var bodies [][]byte
	for i := 0; i < n; i++ {
		x := numeric[nb[0][0]:nb[0][1]]
		if i%2 == 1 {
			x = control[cb[1][0]:cb[1][1]] // () -> (): unreachable
		}
		bodies = append(bodies, cat(uleb(uint64(len(x))), x))
	}
	return modSpec{types: [][]byte{voidFn}, bodies: bodies}.bytes()
}

func BenchmarkValidateFunctions(b *testing.B) {
	mod := bigModule(1000)
	l := V8Limits()
	m, err := Decode(mod, l)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(mod)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ValidateFunctions(m, l); err != nil {
			b.Fatal(err)
		}
	}
}

// TestValidateAllocations checks that validating does not allocate per
// function or per instruction.
func TestValidateAllocations(t *testing.T) {
	mod := bigModule(200)
	l := V8Limits()
	m, err := Decode(mod, l)
	if err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(5, func() {
		if err := ValidateFunctions(m, l); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 20 {
		t.Errorf("validating 200 functions allocated %v times", allocs)
	}
}
