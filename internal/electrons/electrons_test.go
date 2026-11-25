package electrons

import "testing"

func TestSplitAddsUp(t *testing.T) {
	got := Split(100, []int64{1, 1, 1})
	if got[0]+got[1]+got[2] != 100 || got[0] != 34 {
		t.Fatalf("largest remainder: %v", got)
	}
	if s := Split(7, []int64{0, 0}); s[0]+s[1] != 0 {
		t.Fatalf("zero weights: %v", s)
	}
}
