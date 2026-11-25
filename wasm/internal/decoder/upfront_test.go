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
