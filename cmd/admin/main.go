// Command admin manages admin accounts from the server (FR-ROLE-06). Admins
// cannot sign up on the website; at most 3 may be active.
//
//	admin create --phone 9876543210 --name "Asha Admin"
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/auth"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/config"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "create" {
		fmt.Fprintln(os.Stderr, "usage: admin create --phone <10-digit mobile> --name <name>")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	phone := fs.String("phone", "", "10-digit Indian mobile number")
	name := fs.String("name", "", "display name")
	_ = fs.Parse(os.Args[2:])
	if *phone == "" || *name == "" {
		fs.Usage()
		os.Exit(2)
	}

	cfg, err := config.Load()
	if err != nil {
		fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		fail(err)
	}
	defer pool.Close()

	svc := &auth.Service{Pool: pool, Q: store.New(pool)}
	acc, err := svc.CreateAdmin(ctx, *phone, *name)
	if err != nil {
		fail(err)
	}
	fmt.Printf("admin created: %s (%s). Sign in with this number, then set up the authenticator app.\n", acc.ID, auth.MaskPhone(*acc.PhoneE164))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "admin:", err)
	os.Exit(1)
}
