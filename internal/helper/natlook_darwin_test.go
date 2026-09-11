//go:build darwin

package helper

import (
	"testing"
	"unsafe"
)

func TestNatlookStructSize(t *testing.T) {
	if s := unsafe.Sizeof(pfiocNatlook{}); s != 84 {
		t.Fatalf("pfioc_natlook must be 84 bytes, got %d", s)
	}
	if got := ioctlIOWR('D', 23, 84); got != 0xC0544417 {
		t.Fatalf("DIOCNATLOOK = %#x, want 0xC0544417", got)
	}
}
