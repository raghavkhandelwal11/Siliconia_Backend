package seller

import (
	"regexp"
	"strconv"
	"strings"
)

var gstinRE = regexp.MustCompile(`^[0-9]{2}[A-Z]{5}[0-9]{4}[A-Z][1-9A-Z]Z[0-9A-Z]$`)

const gstinChars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"

// ValidGSTIN checks the format, the state code, the check digit and, when pan
// is given, that the GSTIN was issued to that PAN (characters 3–12).
func ValidGSTIN(gstin, pan string) bool {
	if !gstinRE.MatchString(gstin) {
		return false
	}
	if state, _ := strconv.Atoi(gstin[:2]); state < 1 || (state > 38 && state != 97) {
		return false
	}
	if pan != "" && gstin[2:12] != pan {
		return false
	}
	return gstin[14] == gstinCheckChar(gstin[:14])
}

// gstinCheckChar computes the 15th character (base-36 Luhn mod N).
func gstinCheckChar(first14 string) byte {
	sum := 0
	for i := 0; i < 14; i++ {
		v := strings.IndexByte(gstinChars, first14[i])
		factor := 1
		if i%2 == 1 {
			factor = 2
		}
		p := v * factor
		sum += p/36 + p%36
	}
	return gstinChars[(36-sum%36)%36]
}
