package main

import (
	"time"
	_ "time/tzdata" // embed zone data so distroless images know Asia/Kolkata
)

// mustIST returns India Standard Time; schedules are defined in IST (LLD 1).
func mustIST() *time.Location {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		return time.FixedZone("IST", 5*60*60+30*60)
	}
	return loc
}
