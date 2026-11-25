package admin

import "testing"

func TestCSVCellNeutralisesFormulas(t *testing.T) {
	for in, want := range map[string]string{"=HYPERLINK(1)": "'=HYPERLINK(1)", "+91": "'+91", "@x": "'@x", "Volt": "Volt", "": ""} {
		if got := csvCell(in); got != want {
			t.Errorf("csvCell(%q) = %q, want %q", in, got, want)
		}
	}
}
