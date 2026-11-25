package ship

import (
	"bytes"
	"fmt"
	"strings"
)

// Label renders a 4×6 inch shipping label as a one-page PDF. It is written
// by hand (text only, built-in Helvetica) so the simulator needs no PDF library.
func Label(sh Shipment) []byte {
	cod := "PREPAID"
	if sh.CODAmount > 0 {
		cod = "COD  Collect " + rupeesText(sh.CODAmount)
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

	var content bytes.Buffer
	y := 410
	for _, l := range lines {
		font := "F1"
		if l.bold {
			font = "F2"
		}
		y -= l.size + 6
		if l.size >= 20 || l.text == "DELIVER TO" || l.text == "RETURN / PICKUP FROM" {
			y -= 6
		}
		fmt.Fprintf(&content, "BT /%s %d Tf 18 %d Td (%s) Tj ET\n", font, l.size, y, pdfText(l.text))
	}
	fmt.Fprintf(&content, "2 w 12 12 264 408 re S\n")

	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 288 432] /Resources << /Font << /F1 4 0 R /F2 5 0 R >> >> /Contents 6 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold /Encoding /WinAnsiEncoding >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", content.Len(), content.String()),
	}
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return out.Bytes()
}

// pdfText escapes a string for a PDF literal and drops characters outside
// printable ASCII (the label uses the standard fonts only).
func pdfText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '(' || r == ')' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r >= 32 && r < 127:
			b.WriteRune(r)
		default:
			b.WriteByte('?')
		}
	}
	return b.String()
}

func rupeesText(paise int64) string {
	r := fmt.Sprintf("%d", paise/100)
	if len(r) > 3 {
		head, tail := r[:len(r)-3], r[len(r)-3:]
		var parts []string
		for len(head) > 2 {
			parts = append([]string{head[len(head)-2:]}, parts...)
			head = head[:len(head)-2]
		}
		if head != "" {
			parts = append([]string{head}, parts...)
		}
		r = strings.Join(parts, ",") + "," + tail
	}
	return "Rs " + r
}
