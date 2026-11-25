package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/outbox"
	paymentprovider "github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/payment"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// Monitor runs synthetic checks every five minutes (LLD 12.4). Two failures
// in a row open an incident and alert admins; a pass resolves it.
type Monitor struct {
	Svc    *Service
	APIURL string            // e.g. http://api:8080
	Sims   map[string]string // name → base URL with /healthz
	Client *http.Client
}

// Check is one synthetic probe.
type Check struct {
	Name string
	Run  func(ctx context.Context) error
}

func (m *Monitor) get(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "siliconics-synthetic/1")
	res, err := m.Client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
		return nil
	}
	return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out)
}

// Checks lists the probes: API readiness, catalogue, search, a product page
// and each simulator.
func (m *Monitor) Checks() []Check {
	api := m.APIURL + "/api/v1"
	cs := []Check{
		{"api", func(ctx context.Context) error { return m.get(ctx, m.APIURL+"/readyz", nil) }},
		{"catalogue", func(ctx context.Context) error { return m.get(ctx, api+"/categories", nil) }},
		{"search", func(ctx context.Context) error { return m.get(ctx, api+"/products?q=phone", nil) }},
		{"product", func(ctx context.Context) error {
			var res struct {
				Items []struct {
					Slug string `json:"slug"`
				} `json:"items"`
			}
			if err := m.get(ctx, api+"/products", &res); err != nil {
				return err
			}
			if len(res.Items) == 0 {
				return nil // an empty catalogue is not an outage
			}
			return m.get(ctx, api+"/products/"+url.PathEscape(res.Items[0].Slug), nil)
		}},
	}
	for name, base := range m.Sims {
		u := base + "/healthz"
		cs = append(cs, Check{name, func(ctx context.Context) error { return m.get(ctx, u, nil) }})
	}
	return cs
}

// Run executes every check once and records the results.
func (m *Monitor) Run(ctx context.Context) error {
	for _, c := range m.Checks() {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		start := time.Now()
		err := c.Run(cctx)
		cancel()
		var msg *string
		if err != nil {
			s := err.Error()
			msg = &s
		}
		if err := m.Svc.Record(ctx, c.Name, err == nil, int32(time.Since(start).Milliseconds()), msg); err != nil {
			return err
		}
	}
	return nil
}

// Record stores one result and opens or resolves the check's incident.
func (s *Service) Record(ctx context.Context, check string, ok bool, latency int32, msg *string) error {
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		if err := q.AddRun(ctx, store.AddRunParams{ID: newID(), CheckName: check, Ok: ok, LatencyMs: latency, Error: msg}); err != nil {
			return err
		}
		if ok {
			_, err := q.ResolveIncident(ctx, check)
			return err
		}
		recent, err := q.RecentRuns(ctx, check)
		if err != nil {
			return err
		}
		if len(recent) < 2 || recent[0] || recent[1] {
			return nil
		}
		return s.openIncident(ctx, tx, q, check, msg)
	})
}

func (s *Service) openIncident(ctx context.Context, tx pgx.Tx, q *store.Queries, check string, msg *string) error {
	n, err := q.OpenIncident(ctx, store.OpenIncidentParams{ID: newID(), CheckName: check, LastError: msg})
	if err != nil || n == 0 { // already open: alert once
		return err
	}
	_, err = outbox.Add(ctx, tx, "health.incident_opened", check, map[string]any{"check": check, "error": deref(msg)})
	return err
}

// CheckLedger compares every account's ledger with its lots. A mismatch
// opens the "ledger-invariant" incident (daily job).
func (s *Service) CheckLedger(ctx context.Context) (int, error) {
	bad, err := s.Q.LedgerMismatches(ctx)
	if err != nil {
		return 0, err
	}
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		if len(bad) == 0 {
			_, err := q.ResolveIncident(ctx, "ledger-invariant")
			return err
		}
		msg := fmt.Sprintf("%d account(s) differ, first %s: ledger %d vs lots %d", len(bad), bad[0].AccountID, bad[0].Ledger, bad[0].Available)
		return s.openIncident(ctx, tx, q, "ledger-invariant", &msg)
	})
	return len(bad), err
}

// Settler reads the gateway's daily settlement report.
type Settler interface {
	Settlement(ctx context.Context, date string) ([]paymentprovider.Settled, error)
}

var ist = time.FixedZone("IST", 19800)

// Reconcile matches our captured payments for an IST day against the
// gateway's settlement report and records every difference (daily job).
func (s *Service) Reconcile(ctx context.Context, gw Settler, day time.Time) (int, error) {
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, ist)
	end := start.AddDate(0, 0, 1)
	date := start.Format("2006-01-02")
	theirs, err := gw.Settlement(ctx, date)
	if err != nil {
		return 0, err
	}
	ours, err := s.Q.CapturedOn(ctx, store.CapturedOnParams{DayStart: &start, DayEnd: &end})
	if err != nil {
		return 0, err
	}
	mine := map[string]int64{}
	for _, p := range ours {
		mine[p.Ref] = p.AmountPaise
	}
	type issue struct{ kind, ref, detail string }
	var issues []issue
	seen := map[string]bool{}
	for _, t := range theirs {
		seen[t.Ref] = true
		amt, ok := mine[t.Ref]
		switch {
		case !ok:
			issues = append(issues, issue{"MISSING_HERE", t.Ref, fmt.Sprintf("Gateway captured %d paise; we have no captured payment", t.Amount)})
		case amt != t.Amount:
			issues = append(issues, issue{"AMOUNT_MISMATCH", t.Ref, fmt.Sprintf("Gateway %d paise, ours %d paise", t.Amount, amt)})
		}
	}
	for ref, amt := range mine {
		if !seen[ref] {
			issues = append(issues, issue{"MISSING_AT_GATEWAY", ref, fmt.Sprintf("We captured %d paise; not in the gateway report", amt)})
		}
	}
	if len(issues) == 0 {
		return 0, nil
	}
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Q.WithTx(tx)
		d := pgtype.Date{Time: time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC), Valid: true}
		for _, i := range issues {
			if err := q.AddReconIssue(ctx, store.AddReconIssueParams{ID: newID(), Day: d, Kind: i.kind, Ref: i.ref, Detail: i.detail}); err != nil {
				return err
			}
		}
		msg := fmt.Sprintf("%d payment difference(s) on %s", len(issues), date)
		return s.openIncident(ctx, tx, q, "reconciliation", &msg)
	})
	return len(issues), err
}

// Purge removes data past its retention period (daily job, DPDP minimisation).
func (s *Service) Purge(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	steps := []struct {
		name string
		fn   func(context.Context) (int64, error)
	}{
		{"otp_challenges", s.Q.PurgeOTP},
		{"webhook_events", s.Q.PurgeWebhooks},
		{"notifications", s.Q.PurgeNotifications},
		{"synthetic_runs", s.Q.PurgeRuns},
		{"sessions", s.Q.PurgeSessions},
		{"outbox", s.Q.PurgeOutbox},
	}
	var errs []error
	for _, st := range steps {
		n, err := st.fn(ctx)
		out[st.name] = n
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", st.name, err))
		}
	}
	return out, errors.Join(errs...)
}
