package ship

import "regexp"

// Serviceability is what the courier network offers for a PIN code.
type Serviceability struct {
	Pincode     string `json:"pincode"`
	Deliverable bool   `json:"deliverable"`
	COD         bool   `json:"cod"`
	ETADays     int    `json:"etaDays"`
	Region      string `json:"region,omitempty"`
}

// region is an India Post postal circle, keyed by the first two digits.
type region struct {
	name   string
	metro  bool // next-day hubs
	remote bool // hills, islands and the north-east: slower, prepaid only
}

// Postal circles by PIN prefix (India Post). Prefixes that are not listed
// (29, 35, 54, 55, 65, 66, 86–89, and 90–99 for the Army Postal Service) are
// not served. The real aggregators use a table of ~19k PINs; the circle rule
// keeps the prototype free of a large data file while behaving the same way.
var regions = func() map[string]region {
	m := map[string]region{}
	add := func(name string, metro, remote bool, prefixes ...string) {
		for _, p := range prefixes {
			m[p] = region{name, metro, remote}
		}
	}
	add("Delhi", true, false, "11")
	add("Haryana", false, false, "12", "13")
	add("Punjab", false, false, "14", "15", "16")
	add("Himachal Pradesh", false, true, "17")
	add("Jammu and Kashmir", false, true, "18", "19")
	add("Uttar Pradesh", false, false, "20", "21", "22", "23", "27", "28")
	add("Uttarakhand", false, true, "24", "26")
	add("Uttar Pradesh", false, false, "25")
	add("Rajasthan", false, false, "30", "31", "32", "33", "34")
	add("Gujarat", false, false, "36", "37", "38", "39")
	add("Maharashtra", true, false, "40")
	add("Maharashtra", false, false, "41", "42", "43", "44")
	add("Madhya Pradesh", false, false, "45", "46", "47", "48")
	add("Chhattisgarh", false, false, "49")
	add("Telangana", true, false, "50")
	add("Andhra Pradesh", false, false, "51", "52", "53")
	add("Karnataka", true, false, "56")
	add("Karnataka", false, false, "57", "58", "59")
	add("Tamil Nadu", true, false, "60")
	add("Tamil Nadu", false, false, "61", "62", "63", "64")
	add("Kerala", false, false, "67", "68", "69")
	add("West Bengal", true, false, "70")
	add("West Bengal", false, false, "71", "72", "73", "74")
	add("Odisha", false, false, "75", "76", "77")
	add("Assam", false, true, "78")
	add("North East", false, true, "79")
	add("Bihar", false, false, "80", "81", "82", "84", "85")
	add("Jharkhand", false, false, "83")
	return m
}()

// Test PIN codes (LLD 12, docs/free-services.md).
const (
	PinRTO            = "999999" // delivery fails twice, then returns to origin
	PinFailedThenOK   = "999998" // one failed attempt, then delivered
	PinNotServiceable = "999997" // looks valid, nobody delivers there
	PinPrepaidOnly    = "999996" // deliverable, no cash on delivery
)

var pinRE = regexp.MustCompile(`^[1-9][0-9]{5}$`)

// Check returns the serviceability of a PIN code.
func Check(pin string) Serviceability {
	out := Serviceability{Pincode: pin}
	if !pinRE.MatchString(pin) {
		return out
	}
	switch pin {
	case PinRTO, PinFailedThenOK:
		return Serviceability{Pincode: pin, Deliverable: true, COD: true, ETADays: 3, Region: "Test"}
	case PinPrepaidOnly:
		return Serviceability{Pincode: pin, Deliverable: true, ETADays: 3, Region: "Test"}
	case PinNotServiceable:
		return out
	}
	r, ok := regions[pin[:2]]
	if !ok {
		return out
	}
	out.Deliverable, out.Region = true, r.name
	switch {
	case r.metro:
		out.COD, out.ETADays = true, 2
	case r.remote:
		out.COD, out.ETADays = false, 6
	default:
		out.COD, out.ETADays = true, 4
	}
	return out
}
