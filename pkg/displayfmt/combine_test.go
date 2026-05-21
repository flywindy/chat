package displayfmt

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCombineWithFallback(t *testing.T) {
	tests := []struct {
		name              string
		first, second, fb string
		want              string
	}{
		{"both present", "Eng", "中", "x", "Eng 中"},
		{"only first", "Eng", "", "x", "Eng"},
		{"only second", "", "中", "x", "中"},
		{"both empty", "", "", "fallback", "fallback"},
		{"equal halves", "Same", "Same", "x", "Same"},
		{"first whitespace only", "   ", "中", "x", "中"},
		{"second whitespace only", "Eng", "   ", "x", "Eng"},
		{"both whitespace only", "   ", "  ", "fallback", "fallback"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, CombineWithFallback(tc.first, tc.second, tc.fb))
		})
	}
}
