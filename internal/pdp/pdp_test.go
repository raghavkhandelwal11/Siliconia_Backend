package pdp

import "testing"

func TestElectronRules(t *testing.T) {
	r := ElectronRules{NonMemberMinPaise: 250000, NonMemberBps: 50, MemberMinPaise: 80000, MemberBps: 200}
	cases := []struct {
		price  int64
		member bool
		want   int64
	}{
		{249900, false, 0},   // below ₹2,500 for non-members
		{250000, false, 13},  // 0.5% of ₹2,500 = 12.5 → rounded up
		{1899900, false, 95}, // 0.5% of ₹18,999 = 94.995 → 95
		{79900, true, 0},     // below ₹800 for members
		{80000, true, 16},    // 2% of ₹800
		{1899900, true, 380}, // 2% of ₹18,999 = 379.98 → 380
	}
	for _, c := range cases {
		if got := r.Earn(c.price, c.member); got != c.want {
			t.Errorf("Earn(%d, member=%v) = %d, want %d", c.price, c.member, got, c.want)
		}
	}
}
