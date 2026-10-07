package vm

import "testing"

// TestElemTablesAgree pins what the element fast paths derive from elemInfos
// to elemInfos itself: elemShifts gives each type's size as a power of two,
// and setElem tells a BigInt type by its kind's place after elemBigInt64.
// An element type added to one table and not the others fails here.
func TestElemTablesAgree(t *testing.T) {
	for k, info := range elemInfos {
		if info.name == "" {
			continue
		}
		if got := 1 << (elemShifts[k&15] & 7); got != info.size {
			t.Errorf("%s: elemShifts gives %d bytes, elemInfos %d", info.name, got, info.size)
		}
		if big := elemType(k) >= elemBigInt64; big != info.big {
			t.Errorf("%s: kind >= elemBigInt64 is %v, elemInfos.big %v", info.name, big, info.big)
		}
	}
	if len(elemInfos) > len(elemShifts) {
		t.Errorf("%d element types, elemShifts has room for %d", len(elemInfos), len(elemShifts))
	}
}
