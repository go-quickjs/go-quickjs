package decoder

import (
	"reflect"
	"testing"
)

// streamVerdict decodes b through a Stream, written in chunks of size
// chunk (all at once for 0), and returns the module and what compiling it
// gives, as verdict says it.
func streamVerdict(b []byte, chunk int, hint int) (string, *Stream) {
	l := V8Limits()
	s := NewStream(l, hint)
	if chunk == 0 {
		s.Write(b)
	} else {
		for i := 0; i < len(b); i += chunk {
			s.Write(b[i:min(i+chunk, len(b))])
		}
	}
	_, err := s.Finish()
	if err != nil {
		return "WebAssembly.Module(): " + jsString(err.Error()), s
	}
	return "OK", s
}

// TestStreamMatchesDecode pins that a module decoded as its bytes arrive is
// what Decode and ValidateFunctions make of the whole -- the same module, or
// the same error at the same offset -- however the bytes are cut, over the
// corpus TestCompareNode compares with Node: the hand-written cases, the
// seeds and their mutations.
func TestStreamMatchesDecode(t *testing.T) {
	cases := corpus()
	if testing.Short() {
		cases = cases[:min(len(cases), 2000)]
	}
	streamed := 0
	for _, c := range cases {
		want := verdict(c.b)
		var wantModule any
		if want == "OK" {
			l := V8Limits()
			m, _ := Decode(c.b, l)
			ValidateFunctions(m, l)
			wantModule = m
		}
		for _, chunk := range []int{0, 1, 3, 17, 256} {
			got, s := streamVerdict(c.b, chunk, len(c.b)*(chunk%2))
			if got != want {
				t.Errorf("%s, chunks of %d:\n got  %s\n want %s", c.name, chunk, got, want)
				continue
			}
			if want != "OK" {
				continue
			}
			m, _ := s.Finish()
			if !reflect.DeepEqual(m, wantModule) {
				t.Errorf("%s, chunks of %d: the module differs from Decode's", c.name, chunk)
			}
			if !s.stopped {
				streamed++
			}
		}
	}
	// A valid module is decoded as it arrives, not again at the end.
	if streamed == 0 {
		t.Error("no module was decoded as it arrived")
	}
	t.Logf("%d modules, %d valid decodings made as the bytes arrived", len(cases), streamed)
}

// TestStreamDecodesAsBytesArrive pins that a function body is validated as
// soon as its bytes are there, before the rest of the module has arrived,
// and that an error there stops the stream, which Finish then reports as
// Decode would.
func TestStreamDecodesAsBytesArrive(t *testing.T) {
	b := parallelModule(t, 40, nil)
	bad := parallelModule(t, 40, map[int]string{3: "drop"})
	l := V8Limits()
	for _, tc := range []struct {
		b    []byte
		ok   bool
		name string
	}{{b, true, "valid"}, {bad, false, "invalid"}} {
		s := NewStream(l, 0)
		half := len(tc.b) / 2
		s.Write(tc.b[:half])
		if tc.ok && (!s.inCode || s.codeNext == 0) {
			t.Errorf("%s: after half the bytes, %d bodies validated, in the code section %v", tc.name, s.codeNext, s.inCode)
		}
		if !tc.ok && !s.stopped {
			t.Errorf("%s: the stream did not stop at the invalid body", tc.name)
		}
		s.Write(tc.b[half:])
		_, err := s.Finish()
		want := verdict(tc.b)
		got := "OK"
		if err != nil {
			got = "WebAssembly.Module(): " + jsString(err.Error())
		}
		if got != want {
			t.Errorf("%s: got %s, want %s", tc.name, got, want)
		}
	}
}
