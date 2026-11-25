// Package pdf writes simple one-page PDFs (text, lines, boxes) with the
// built-in Helvetica fonts, so labels and invoices need no PDF library.
// Text is limited to printable ASCII; other characters become '?'.
package pdf

import (
	"bytes"
	"fmt"
	"strings"
)

// Page is one page being drawn. Coordinates are points from the bottom left.
type Page struct {
	W, H    float64
	content bytes.Buffer
}

// New starts a page of the given size in points (A4 is 595×842).
func New(w, h float64) *Page { return &Page{W: w, H: h} }

// Text draws a line of text with its baseline at (x, y).
func (p *Page) Text(x, y float64, size float64, bold bool, s string) {
	font := "F1"
	if bold {
		font = "F2"
	}
	fmt.Fprintf(&p.content, "BT /%s %.1f Tf %.1f %.1f Td (%s) Tj ET\n", font, size, x, y, Escape(s))
}

// TextRight draws text ending at x (approximate width for Helvetica).
func (p *Page) TextRight(x, y float64, size float64, bold bool, s string) {
	p.Text(x-Width(s, size, bold), y, size, bold, s)
}

// Line draws a straight line.
func (p *Page) Line(x1, y1, x2, y2, width float64) {
	fmt.Fprintf(&p.content, "%.1f w %.1f %.1f m %.1f %.1f l S\n", width, x1, y1, x2, y2)
}

// Rect draws a rectangle outline.
func (p *Page) Rect(x, y, w, h, width float64) {
	fmt.Fprintf(&p.content, "%.1f w %.1f %.1f %.1f %.1f re S\n", width, x, y, w, h)
}

// Fill draws a filled grey rectangle (0 black … 1 white).
func (p *Page) Fill(x, y, w, h, grey float64) {
	fmt.Fprintf(&p.content, "q %.2f g %.1f %.1f %.1f %.1f re f Q\n", grey, x, y, w, h)
}

// Bytes returns the finished PDF document.
func (p *Page) Bytes() []byte {
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.0f %.0f] /Resources << /Font << /F1 4 0 R /F2 5 0 R >> >> /Contents 6 0 R >>", p.W, p.H),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold /Encoding /WinAnsiEncoding >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", p.content.Len(), p.content.String()),
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

// Escape makes a string safe inside a PDF literal string.
func Escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '(' || r == ')' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '₹':
			b.WriteString("Rs ")
		case r >= 32 && r < 127:
			b.WriteRune(r)
		default:
			b.WriteByte('?')
		}
	}
	return b.String()
}

// Width estimates the width of s in points (Helvetica averages ~0.52 em; bold ~0.56).
func Width(s string, size float64, bold bool) float64 {
	em := 0.52
	if bold {
		em = 0.56
	}
	return float64(len(Escape(s))) * size * em
}

// Rupees formats paise in Indian grouping for documents: "Rs 1,23,456.78".
func Rupees(paise int64) string {
	neg := paise < 0
	if neg {
		paise = -paise
	}
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
	out := fmt.Sprintf("Rs %s.%02d", r, paise%100)
	if neg {
		out = "-" + out
	}
	return out
}
