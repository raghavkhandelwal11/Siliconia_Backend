package ship

import (
	"fmt"
	"strings"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/pdf"
)

// Label renders a 4×6 inch shipping label as a one-page PDF. It is written
// by hand (text only, built-in Helvetica) so the simulator needs no PDF library.
func Label(sh Shipment) []byte {
	cod := "PREPAID"
	if sh.CODAmount > 0 {
		cod = "COD  Collect " + strings.TrimSuffix(pdf.Rupees(sh.CODAmount), ".00")
	}
	weight := 0
	for _, p := range sh.Parcels {
		weight += p.WeightGrams
	}
	type line struct {
		size int
		bold bool
		text string
	}
	lines := []line{
		{8, true, "SILICONICS . sim-ship . TEST LABEL, NOT FOR REAL USE"},
		{22, true, sh.AWB},
		{11, true, cod},
		{9, false, fmt.Sprintf("%s  .  %d g", sh.Kind, weight)},
		{7, false, "Ref " + sh.Ref},
		{9, true, "DELIVER TO"},
	}
	addr := func(a Address) {
		lines = append(lines, line{11, true, a.Name})
		for _, l := range []string{a.Line1, a.Line2, a.City + ", " + a.State, "PIN " + a.Pincode, "Phone " + a.Phone} {
			if strings.TrimSpace(strings.Trim(l, ", ")) != "" {
				lines = append(lines, line{10, false, l})
			}
		}
	}
	addr(sh.To)
	lines = append(lines, line{9, true, "RETURN / PICKUP FROM"})
	addr(sh.From)
	lines = append(lines, line{8, false, "Expected by " + sh.ETA.Format("02 Jan 2006")})

	p := pdf.New(288, 432)
	y := 410.0
	for _, l := range lines {
		y -= float64(l.size) + 6
		if l.size >= 20 || l.text == "DELIVER TO" || l.text == "RETURN / PICKUP FROM" {
			y -= 6
		}
		p.Text(18, y, float64(l.size), l.bold, l.text)
	}
	p.Rect(12, 12, 264, 408, 2)
	return p.Bytes()
}
