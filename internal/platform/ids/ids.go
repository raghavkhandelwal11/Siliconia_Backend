// Package ids makes short, readable reference numbers for people to quote,
// e.g. RT-261126-K7Q2M. Uniqueness is enforced by the database.
package ids

import (
	"crypto/rand"
	"math/big"
	"time"
)

// alphabet avoids look-alike characters (0/O, 1/I).
const alphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

var ist = time.FixedZone("IST", 19800)

// Readable returns prefix-yymmdd-XXXXX with the date in IST.
func Readable(prefix string, t time.Time) string {
	b := make([]byte, 5)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b[i] = alphabet[n.Int64()]
	}
	return prefix + "-" + t.In(ist).Format("060102") + "-" + string(b)
}
