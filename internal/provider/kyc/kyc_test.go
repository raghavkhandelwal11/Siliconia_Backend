package kyc_test

import (
	"context"
	"net/http/httptest"
	"testing"

	provider "github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/kyc"
	sim "github.com/raghavkhandelwal11/Siliconia_Backend/internal/sim/kyc"
)

func TestNameScore(t *testing.T) {
	cases := []struct {
		a, b string
		pass bool
	}{
		{"Asha Verma", "ASHA VERMA", true},
		{"Mr. Asha  Verma", "asha verma", true},
		{"Verma Asha", "Asha Verma", true},
		{"Asha Varma", "Asha Verma", true}, // one-letter typo
		{"Asha Verma", "Ramesh Kumar Verma", false},
		{"Asha Verma", "", false},
	}
	for _, c := range cases {
		if got := provider.NameScore(c.a, c.b) >= provider.MinNameMatch; got != c.pass {
			t.Errorf("NameScore(%q, %q) = %.2f, pass=%v", c.a, c.b, provider.NameScore(c.a, c.b), got)
		}
	}
}

func TestSimRules(t *testing.T) {
	srv := httptest.NewServer(sim.Server{}.Handler())
	defer srv.Close()
	p := provider.NewSim(srv.URL)
	ctx := context.Background()

	check := func(name string, r provider.Result, err error, want provider.Status) {
		t.Helper()
		if err != nil || r.Status != want || r.Reference == "" {
			t.Errorf("%s: %+v %v, want %s", name, r, err, want)
		}
	}
	r, err := p.VerifyPAN(ctx, "ABCDE1234F", "Asha Verma")
	check("good PAN", r, err, provider.Verified)
	if r.NameMatch != 1 {
		t.Errorf("name match: %v", r.NameMatch)
	}
	r, err = p.VerifyPAN(ctx, "AAAAA0000A", "Asha Verma")
	check("unknown PAN", r, err, provider.Failed)
	r, err = p.VerifyPAN(ctx, "abc", "Asha Verma")
	check("bad PAN", r, err, provider.Failed)

	r, err = p.PennyDrop(ctx, "123456781234", "HDFC0001234", "Asha Verma")
	check("good account", r, err, provider.Verified)
	r, err = p.PennyDrop(ctx, "123456780000", "HDFC0001234", "Asha Verma")
	check("someone else's account", r, err, provider.NameMismatch)
	r, err = p.PennyDrop(ctx, "123456789999", "HDFC0001234", "Asha Verma")
	check("closed account", r, err, provider.Failed)
	r, err = p.PennyDrop(ctx, "12345", "HDFC0001234", "Asha Verma")
	check("short account", r, err, provider.Failed)
}
