package decoder

import (
	"runtime"
	"testing"
)

// TestExportCountAllocatesBySize checks that a section's declared count
// does not decide alone what is allocated before its entries are read: an
// export section of a few bytes declaring the limit's million exports
// allocates for what its bytes can hold.
func TestExportCountAllocatesBySize(t *testing.T) {
	b := []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x07, 0x04, // the export section, 4 bytes
		0xc0, 0x84, 0x3d, // 1,000,000 exports
		0x00, // the first name's length, and no more
	}
	l := V8Limits()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := Decode(b, l)
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Fatal("a truncated export section decoded")
	}
	if n := after.TotalAlloc - before.TotalAlloc; n > 64<<10 {
		t.Errorf("decoding allocated %d bytes; want what the section's bytes can hold", n)
	}
}

// decodeAllocs decodes b under l and returns its error and the bytes it
// allocated.
func decodeAllocs(t *testing.T, b []byte, l *Limits) (*Error, uint64) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := Decode(b, l)
	runtime.ReadMemStats(&after)
	return err, after.TotalAlloc - before.TotalAlloc
}

// typeSectionModule returns a module whose type section is body.
func typeSectionModule(body ...byte) []byte {
	b := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x01, byte(len(body))}
	return append(b, body...)
}

// TestTypeCountsAllocateBySize checks that a recursion group's size and a
// struct's field count do not decide alone what is allocated before their
// entries are read, and that the errors are those of reading the entries.
func TestTypeCountsAllocateBySize(t *testing.T) {
	cases := []struct {
		name string
		b    []byte
		err  string
	}{
		{
			// One group of 2^18 types, and no definition.
			name: "recursion group",
			b:    typeSectionModule(0x01, 0x4e, 0x80, 0x80, 0x10),
			err:  "type kind @+15",
		},
		{
			// One struct of 2^18 fields, and no field.
			name: "struct fields",
			b:    typeSectionModule(0x01, 0x5f, 0x80, 0x80, 0x10),
			err:  "expected 1 byte @+15",
		},
		{
			// A group of 2^18 types whose first is a struct with a field
			// naming the group's last type, and no more: the name is in
			// bounds, and the error is the end of the section.
			name: "group's last type",
			b:    typeSectionModule(0x01, 0x4e, 0x80, 0x80, 0x10, 0x5f, 0x01, 0x63, 0xff, 0xff, 0x0f, 0x00),
			err:  "type kind @+22",
		},
		{
			// The same naming the type after the group's last.
			name: "past the group",
			b:    typeSectionModule(0x01, 0x4e, 0x80, 0x80, 0x10, 0x5f, 0x01, 0x63, 0x80, 0x80, 0x10, 0x00),
			err:  "Type index 262144 is out of bounds @+17",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err, n := decodeAllocs(t, c.b, CoreLimits())
			if err == nil {
				t.Fatal("decoded")
			}
			if got := err.Error(); got != c.err {
				t.Errorf("error %q, want %q", got, c.err)
			}
			t.Logf("allocated %d bytes", n)
			if n > 64<<10 {
				t.Errorf("decoding allocated %d bytes; want what the section's bytes can hold", n)
			}
		})
	}
}
