package decoder

import "testing"

// TestLastCustomSectionOnce pins that a module whose last section is a
// custom section the decoder knows -- name, sourceMappingURL -- lists it
// once among its custom sections, as it lists any other.
func TestLastCustomSectionOnce(t *testing.T) {
	for _, names := range [][]string{{"name"}, {"sourceMappingURL"}, {"x", "name"}, {"name", "x"}, {"other"}} {
		b := []byte{0, 0x61, 0x73, 0x6d, 1, 0, 0, 0}
		for _, n := range names {
			payload := append(append([]byte{byte(len(n))}, n...), 1, 2)
			b = append(append(b, 0, byte(len(payload))), payload...)
		}
		m, err := Decode(b, V8Limits())
		if err != nil {
			t.Fatal(err)
		}
		if len(m.Customs) != len(names) {
			t.Errorf("%v: %d custom sections: %v", names, len(m.Customs), m.Customs)
		}
	}
}
