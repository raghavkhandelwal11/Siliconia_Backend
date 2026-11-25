package orders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/pdf"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

var ist = time.FixedZone("IST", 19800)

// Viewer is who asks for an invoice.
type Viewer struct {
	ID    uuid.UUID
	Admin bool
}

// invoiced lists the statuses after which goods have left the seller.
var invoiced = map[store.SubStatus]bool{
	store.SubStatusSHIPPED: true, store.SubStatusOUTFORDELIVERY: true, store.SubStatusDELIVERED: true, store.SubStatusRTO: true,
}

// Invoice returns the GST tax invoice PDF for a sub-order (buyer, seller or
// admin). The number is issued from the seller's own series the first time
// it is asked for after shipping.
func (s *Service) Invoice(ctx context.Context, v Viewer, id uuid.UUID) ([]byte, string, error) {
	var d store.GetSubOrderDetailRow
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		so, err := q.GetSubOrderForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if d, err = q.GetSubOrderDetail(ctx, id); err != nil {
			return err
		}
		if !v.Admin && v.ID != d.BuyerID && v.ID != d.SellerID {
			return httpx.ErrNotFound
		}
		if !invoiced[so.Status] {
			return ErrNoInvoice
		}
		if d.InvoiceNo != nil {
			return nil
		}
		n, err := q.NextInvoiceNumber(ctx, d.SellerID)
		if err != nil {
			return err
		}
		no := invoiceNumber(d.SellerID, time.Now(), n)
		if err := q.SetInvoice(ctx, store.SetInvoiceParams{ID: id, InvoiceNo: &no}); err != nil {
			return err
		}
		d.InvoiceNo = &no
		now := time.Now()
		d.InvoicedAt = &now
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	items, err := s.Q.ListSubOrderItems(ctx, id)
	if err != nil {
		return nil, "", err
	}
	return renderInvoice(d, items), *d.InvoiceNo, nil
}

// invoiceNumber is the seller's own series per financial year (April–March),
// e.g. SX-1A2B/2627/00042 (at most 16 characters after the prefix, as GST allows).
func invoiceNumber(seller uuid.UUID, t time.Time, n int32) string {
	t = t.In(ist)
	fy := t.Year() % 100
	if t.Month() < time.April {
		fy--
	}
	return fmt.Sprintf("SX-%s/%02d%02d/%05d", strings.ToUpper(seller.String()[:4]), fy, (fy+1)%100, n)
}

type taxLine struct {
	name, hsn          string
	qty                int32
	unit, taxable, tax int64
	rateBps            int32
	total              int64
}

// split works out the taxable value inside a GST-inclusive line total.
func split(total int64, rateBps int32) (taxable, tax int64) {
	taxable = (total*10000 + int64(10000+rateBps)/2) / int64(10000+rateBps)
	return taxable, total - taxable
}

func renderInvoice(d store.GetSubOrderDetailRow, items []store.OrderItem) []byte {
	var buyer Address
	_ = json.Unmarshal(d.Address, &buyer)
	var from struct{ Line1, Line2, City, State, Pincode string }
	_ = json.Unmarshal(d.PickupAddress, &from)
	intra := strings.EqualFold(strings.TrimSpace(from.State), strings.TrimSpace(buyer.State))

	var lines []taxLine
	var sumTaxable, sumTax, sumTotal int64
	for _, it := range items {
		if it.Status == "CANCELLED" {
			continue
		}
		taxable, tax := split(it.LineTotalPaise, it.GstRateBps)
		name := it.ProductName
		var attrs map[string]string
		if json.Unmarshal(it.VariantAttrs, &attrs) == nil && len(attrs) > 0 {
			keys := make([]string, 0, len(attrs))
			for k := range attrs {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, len(keys))
			for i, k := range keys {
				parts[i] = attrs[k]
			}
			name += " (" + strings.Join(parts, ", ") + ")"
		}
		lines = append(lines, taxLine{name: name, hsn: derefOr(it.Hsn, "-"), qty: it.Qty, unit: it.UnitPricePaise, taxable: taxable,
			tax: tax, rateBps: it.GstRateBps, total: it.LineTotalPaise})
		sumTaxable, sumTax, sumTotal = sumTaxable+taxable, sumTax+tax, sumTotal+it.LineTotalPaise
	}

	p := pdf.New(595, 842)
	// Prototype data must never pass for a real tax document.
	p.Fill(40, 790, 515, 26, 0.88)
	p.Text(50, 799, 9, true, "SAMPLE - Siliconics prototype test data. Not a valid tax invoice.")
	p.Text(40, 760, 18, true, "TAX INVOICE")
	p.TextRight(555, 760, 9, false, "Original for recipient")

	y := 735.0
	p.Text(40, y, 10, true, d.LegalName)
	p.Text(40, y-13, 9, false, "Trading as "+d.StoreName)
	p.Text(40, y-26, 9, false, strings.TrimSpace(from.Line1+" "+from.Line2))
	p.Text(40, y-39, 9, false, fmt.Sprintf("%s, %s %s", from.City, from.State, from.Pincode))
	p.Text(40, y-52, 9, true, "GSTIN: "+derefOr(d.Gstin, "Not registered"))

	date := time.Now()
	if d.InvoicedAt != nil {
		date = *d.InvoicedAt
	}
	meta := [][2]string{
		{"Invoice no.", derefOr(d.InvoiceNo, "")},
		{"Invoice date", date.In(ist).Format("02 Jan 2006")},
		{"Order no.", d.Number},
		{"Order date", d.PlacedAt.In(ist).Format("02 Jan 2006")},
		{"Place of supply", buyer.State},
		{"Payment", map[string]string{"COD": "Cash on delivery", "ONLINE": "Prepaid"}[d.PayMethod]},
	}
	for i, m := range meta {
		p.Text(360, y-float64(i)*13, 9, false, m[0])
		p.Text(445, y-float64(i)*13, 9, true, m[1])
	}

	y = 640
	p.Line(40, y+12, 555, y+12, 0.5)
	p.Text(40, y, 9, true, "Bill to / Ship to")
	p.Text(40, y-13, 10, true, buyer.Name)
	p.Text(40, y-26, 9, false, strings.TrimSpace(buyer.Line1+" "+buyer.Line2))
	p.Text(40, y-39, 9, false, fmt.Sprintf("%s, %s %s", buyer.City, buyer.State, buyer.Pincode))

	// Items table.
	y = 570
	cols := []struct {
		x     float64
		title string
		right bool
	}{{40, "Item", false}, {244, "HSN", false}, {334, "Qty", true}, {406, "Taxable value", true}, {446, "GST", true}, {498, "Tax", true}, {555, "Total", true}}
	p.Fill(40, y-5, 515, 18, 0.93)
	for _, c := range cols {
		if c.right {
			p.TextRight(c.x, y, 8.5, true, c.title)
		} else {
			p.Text(c.x+4, y, 8.5, true, c.title)
		}
	}
	y -= 20
	for _, l := range lines {
		name := l.name
		if len(name) > 38 {
			name = name[:37] + "..."
		}
		p.Text(44, y, 9, false, name)
		p.Text(248, y, 9, false, l.hsn)
		p.TextRight(334, y, 9, false, fmt.Sprint(l.qty))
		p.TextRight(406, y, 9, false, pdf.Rupees(l.taxable))
		p.TextRight(446, y, 9, false, fmt.Sprintf("%g%%", float64(l.rateBps)/100))
		p.TextRight(498, y, 9, false, pdf.Rupees(l.tax))
		p.TextRight(555, y, 9, false, pdf.Rupees(l.total))
		y -= 16
	}
	p.Line(40, y+8, 555, y+8, 0.5)

	// Totals with the tax split by place of supply.
	y -= 10
	row := func(label, value string, bold bool) {
		p.Text(360, y, 9, bold, label)
		p.TextRight(555, y, 9, bold, value)
		y -= 14
	}
	row("Taxable value", pdf.Rupees(sumTaxable), false)
	if intra {
		half := sumTax / 2
		row("CGST", pdf.Rupees(half), false)
		row("SGST", pdf.Rupees(sumTax-half), false)
	} else {
		row("IGST", pdf.Rupees(sumTax), false)
	}
	row("Shipping", pdf.Rupees(0), false)
	p.Line(360, y+10, 555, y+10, 0.5)
	row("Invoice total", pdf.Rupees(sumTotal), true)

	p.Text(40, 90, 8, false, "Prices include GST. Tax is not payable on reverse charge.")
	p.Text(40, 78, 8, false, "Sold by "+d.LegalName+" through the Siliconics marketplace.")
	p.Text(40, 66, 8, false, "This is a computer-generated document and needs no signature.")
	return p.Bytes()
}

func derefOr(s *string, def string) string {
	if s == nil || *s == "" {
		return def
	}
	return *s
}
