package launch

import "testing"

func TestDealUnits(t *testing.T) {
	// LLD 9.1 example: 20 units at ₹14,000 cost, ₹20,000 normal, 80% off, 4% minimum profit.
	k, price, err := DealUnits(20, 1400000, 2000000, 8000, 400)
	if err != nil || k != 6 || price != 400000 {
		t.Fatalf("got k=%d price=%d err=%v", k, price, err)
	}
	if _, _, err := DealUnits(10, 2000000, 2000000, 8000, 400); err != ErrNotViable {
		t.Fatalf("no margin must be refused: %v", err)
	}
	if k, _, _ := DealUnits(5, 100, 2000000, 1000, 0); k != 5 {
		t.Fatalf("capped at units bought: %d", k)
	}
}

func TestAdmissionPass(t *testing.T) {
	s := &Service{Secret: []byte("k")}
	id, acc, other := newID(), newID(), newID()
	p := s.pass(id, acc, s.now().Add(60e9))
	if !s.validPass(p, id, acc) || s.validPass(p, id, other) || s.validPass(p+"x", id, acc) {
		t.Fatal("pass must bind launch and account and be signed")
	}
	if s.validPass(s.pass(id, acc, s.now().Add(-1e9)), id, acc) {
		t.Fatal("expired pass accepted")
	}
}
