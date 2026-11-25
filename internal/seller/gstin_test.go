package seller

import "testing"

func TestValidGSTIN(t *testing.T) {
	good := "27ABCDE1234F1Z" + string(gstinCheckChar("27ABCDE1234F1Z"))
	cases := []struct {
		gstin, pan string
		ok         bool
	}{
		{"27AAPFU0939F1ZV", "", true}, // widely published sample GSTIN
		{good, "ABCDE1234F", true},
		{good, "ABCDE9999F", false},    // issued to a different PAN
		{"27AAPFU0939F1ZX", "", false}, // wrong check character
		{"00AAPFU0939F1ZV", "", false}, // no such state
		{"27AAPFU0939F1Z", "", false},  // too short
		{"27aapfu0939f1zv", "", false}, // lower case
	}
	for _, c := range cases {
		if got := ValidGSTIN(c.gstin, c.pan); got != c.ok {
			t.Errorf("ValidGSTIN(%q, %q) = %v", c.gstin, c.pan, got)
		}
	}
}
