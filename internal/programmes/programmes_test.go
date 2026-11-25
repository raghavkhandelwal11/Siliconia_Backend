package programmes

import "testing"

func TestShares(t *testing.T) {
	// LLD 9.4: pool 1000, scores 120/150/20 → 414/517/69.
	got := Shares(1000, []int64{120, 150, 20})
	if got[0] != 414 || got[1] != 517 || got[2] != 69 {
		t.Fatalf("got %v", got)
	}
	if s := Shares(10, []int64{0, 0}); s[0]+s[1] != 0 {
		t.Fatalf("no votes, no shares: %v", s)
	}
}
