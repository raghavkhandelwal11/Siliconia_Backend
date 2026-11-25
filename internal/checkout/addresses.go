package checkout

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/httpx"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// MaxAddresses a buyer may save.
const MaxAddresses = 10

// Address is a saved delivery address.
type Address struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Phone     string    `json:"phone"`
	Line1     string    `json:"line1"`
	Line2     string    `json:"line2"`
	City      string    `json:"city"`
	State     string    `json:"state"`
	Pincode   string    `json:"pincode"`
	IsDefault bool      `json:"isDefault"`
}

// AddressInput is the address form.
type AddressInput struct {
	Name      string `json:"name"`
	Phone     string `json:"phone"`
	Line1     string `json:"line1"`
	Line2     string `json:"line2"`
	City      string `json:"city"`
	State     string `json:"state"`
	Pincode   string `json:"pincode"`
	IsDefault bool   `json:"isDefault"`
}

// ErrPinNotServed means no courier delivers to the PIN code.
var ErrPinNotServed = httpx.NewError(http.StatusUnprocessableEntity, "PIN_NOT_SERVICEABLE", "We don't deliver to this PIN code yet")

var (
	phoneRE   = regexp.MustCompile(`^[6-9][0-9]{9}$`)
	pincodeRE = regexp.MustCompile(`^[1-9][0-9]{5}$`)
)

func (in *AddressInput) clean() error {
	f := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	in.Name, in.Line1, in.Line2, in.City, in.State = f(in.Name), f(in.Line1), f(in.Line2), f(in.City), f(in.State)
	in.Phone = strings.TrimPrefix(strings.ReplaceAll(strings.ReplaceAll(in.Phone, " ", ""), "-", ""), "+91")
	in.Pincode = strings.TrimSpace(in.Pincode)
	n := func(s string) int { return len([]rune(s)) }
	switch {
	case n(in.Name) < 2 || n(in.Name) > 80:
		return httpx.ErrValidation.WithDetail("Enter the name of the person receiving the order")
	case !phoneRE.MatchString(in.Phone):
		return httpx.ErrValidation.WithDetail("Enter a 10-digit mobile number for delivery updates")
	case n(in.Line1) < 3 || n(in.Line1) > 120 || n(in.Line2) > 120:
		return httpx.ErrValidation.WithDetail("Enter the house, building and street")
	case n(in.City) < 2 || n(in.City) > 60 || n(in.State) < 2 || n(in.State) > 60:
		return httpx.ErrValidation.WithDetail("Enter the city and state")
	case !pincodeRE.MatchString(in.Pincode):
		return httpx.ErrValidation.WithDetail("Enter a 6-digit PIN code")
	}
	return nil
}

func addressView(a store.Address) Address {
	return Address{ID: a.ID, Name: a.Name, Phone: a.Phone, Line1: a.Line1, Line2: a.Line2, City: a.City, State: a.State, Pincode: a.Pincode, IsDefault: a.IsDefault}
}

// Addresses lists the buyer's addresses, default first.
func (s *Service) Addresses(ctx context.Context, buyer uuid.UUID) ([]Address, error) {
	rows, err := s.Q.ListAddresses(ctx, buyer)
	if err != nil {
		return nil, err
	}
	out := make([]Address, len(rows))
	for i, r := range rows {
		out[i] = addressView(r)
	}
	return out, nil
}

// SaveAddress creates (id nil) or updates an address. The first address and
// any address marked default become the only default.
func (s *Service) SaveAddress(ctx context.Context, buyer uuid.UUID, id *uuid.UUID, in AddressInput) (Address, error) {
	if err := in.clean(); err != nil {
		return Address{}, err
	}
	// LLD 7: the PIN is checked against courier serviceability. When the
	// courier service does not answer, the address is still saved.
	if sv := s.pin(ctx, in.Pincode); sv.Known && !sv.Deliverable {
		return Address{}, ErrPinNotServed
	}
	var out store.Address
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		n, err := q.CountAddresses(ctx, buyer)
		if err != nil {
			return err
		}
		if id == nil && n >= MaxAddresses {
			return httpx.NewError(http.StatusConflict, "TOO_MANY_ADDRESSES", "You can save up to 10 addresses. Remove one first")
		}
		makeDefault := in.IsDefault || n == 0
		if makeDefault {
			if err := q.ClearDefaultAddress(ctx, buyer); err != nil {
				return err
			}
		}
		if id == nil {
			newID, _ := uuid.NewV7()
			out, err = q.CreateAddress(ctx, store.CreateAddressParams{ID: newID, AccountID: buyer, Name: in.Name, Phone: in.Phone, Line1: in.Line1,
				Line2: in.Line2, City: in.City, State: in.State, Pincode: in.Pincode, IsDefault: makeDefault})
			return err
		}
		out, err = q.UpdateAddress(ctx, store.UpdateAddressParams{ID: *id, AccountID: buyer, Name: in.Name, Phone: in.Phone, Line1: in.Line1,
			Line2: in.Line2, City: in.City, State: in.State, Pincode: in.Pincode, IsDefault: makeDefault})
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrNotFound
		}
		return err
	})
	return addressView(out), err
}

// DeleteAddress removes an address (past orders keep their snapshot).
func (s *Service) DeleteAddress(ctx context.Context, buyer, id uuid.UUID) error {
	n, err := s.Q.DeleteAddress(ctx, store.DeleteAddressParams{ID: id, AccountID: buyer})
	if err == nil && n == 0 {
		return httpx.ErrNotFound
	}
	return err
}
