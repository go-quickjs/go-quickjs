package normalize

import (
	"slices"
	"testing"
)

func TestNFKCWithOffsets(t *testing.T) {
	for _, tc := range []struct {
		input, normalized string
		offsets           []int
	}{
		{"ｶﾞラス", "ガラス", []int{0, 6, 9, 12}},
		{"ガラス", "ガラス", []int{0, 6, 9, 12}},
		{"㍻時代", "平成時代", []int{0, 0, 3, 6, 9}},
	} {
		got, offsets := NFKCWithOffsets(tc.input)
		if got != tc.normalized || !slices.Equal(offsets, tc.offsets) {
			t.Errorf("NFKCWithOffsets(%q) = %q, %v; want %q, %v",
				tc.input, got, offsets, tc.normalized, tc.offsets)
		}
	}
}
