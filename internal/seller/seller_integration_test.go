package seller_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/auth"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/health"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/config"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/crypto"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/migrate"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/ratelimit"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/kyc"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/messaging"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/seller"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/server"
	simkyc "github.com/raghavkhandelwal11/Siliconia_Backend/internal/sim/kyc"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

type inbox struct {
	mu   sync.Mutex
	msgs []messaging.Message
}

func (b *inbox) Send(_ context.Context, m messaging.Message) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs = append(b.msgs, m)
	return "t", nil
}

var codeRE = regexp.MustCompile(`\b\d{6}\b`)

func (b *inbox) last(to, template string) messaging.Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.msgs) - 1; i >= 0; i-- {
		if b.msgs[i].To == to && b.msgs[i].Template == template {
			return b.msgs[i]
		}
	}
	return messaging.Message{}
}

type env struct {
	t     *testing.T
	srv   *httptest.Server
	auth  *auth.Service
	inbox *inbox
	mr    *miniredis.Miniredis
}

func setup(t *testing.T) *env {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if err := migrate.Up(ctx, dbURL); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE accounts, sessions, otp_challenges, audit_log, seller_profiles, kyc_records, outbox CASCADE`); err != nil {
		t.Fatal(err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	signer, _ := auth.NewSigner(config.DevKey("auth-signing"), auth.AccessTTL)
	box, _ := crypto.NewBox(config.DevKey("encryption-master"))
	in := &inbox{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	limiter := ratelimit.New(rdb)
	authSvc := &auth.Service{Pool: pool, Q: store.New(pool), Redis: rdb, Limiter: limiter, Messages: in, Signer: signer, Box: box, Pepper: []byte("p"), Log: log}
	kycSrv := httptest.NewServer(simkyc.Server{}.Handler())
	t.Cleanup(kycSrv.Close)
	sellers := &seller.Service{Pool: pool, Q: authSvc.Q, Box: box, KYC: kyc.NewSim(kycSrv.URL), Limiter: limiter, Messages: in, Log: log}
	srv := httptest.NewServer(server.New(server.Deps{
		Log: log, Health: &health.Handler{}, AuthService: authSvc, Auth: auth.NewHandler(authSvc, nil, false),
		Seller: &seller.Handler{Svc: sellers},
	}))
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, auth: authSvc, inbox: in, mr: mr}
}

type client struct {
	e   *env
	jar *cookiejar.Jar
}

func (e *env) browser() *client { j, _ := cookiejar.New(nil); return &client{e: e, jar: j} }

func (c *client) do(method, path string, body any) (int, map[string]any) {
	c.e.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.e.srv.URL+"/api/v1"+path, r)
	req.Header.Set("Content-Type", "application/json")
	u, _ := url.Parse(c.e.srv.URL + "/")
	for _, ck := range c.jar.Cookies(u) {
		if ck.Name == auth.CookieCSRF {
			req.Header.Set(auth.HeaderCSRF, ck.Value)
		}
	}
	res, err := (&http.Client{Jar: c.jar}).Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer res.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func (c *client) signIn(phone, role string) {
	c.e.t.Helper()
	c.e.mr.FastForward(time.Minute)
	_, out := c.do("POST", "/auth/otp/send", map[string]string{"phone": phone, "role": role})
	code := codeRE.FindString(c.e.inbox.last("+91"+phone, "otp").Body)
	if st, o := c.do("POST", "/auth/otp/verify", map[string]string{"challengeId": out["challengeId"].(string), "code": code}); st != 200 {
		c.e.t.Fatalf("sign-in %s: %d %v", role, st, o)
	}
}

func (c *client) must(method, path string, body any, want int) map[string]any {
	c.e.t.Helper()
	st, out := c.do(method, path, body)
	if st != want {
		c.e.t.Fatalf("%s %s: got %d want %d: %v", method, path, st, want, out)
	}
	return out
}

func (e *env) admin() *client {
	e.t.Helper()
	if _, err := e.auth.CreateAdmin(context.Background(), "9300000009", "Root"); err != nil {
		e.t.Fatal(err)
	}
	a := e.browser()
	a.signIn("9300000009", "ADMIN")
	setup := a.must("POST", "/auth/totp/setup", nil, 200)
	code, _ := auth.TOTPCode(setup["secret"].(string), time.Now())
	a.must("POST", "/auth/totp/verify", map[string]string{"code": code}, 204)
	return a
}

var business = map[string]any{
	"businessType": "PROPRIETORSHIP", "legalName": "Asha Verma", "storeName": "Asha Electronics",
	"pickupAddress": map[string]string{"line1": "12 MG Road", "city": "Pune", "state": "Maharashtra", "pincode": "411001", "phone": "9876543210"},
}

func TestSellerOnboardingAndApproval(t *testing.T) {
	e := setup(t)
	s := e.browser()
	s.signIn("9876500011", "SELLER")

	app := s.must("GET", "/seller/application", nil, 200)
	if app["approval"] != "DRAFT" || app["editable"] != true {
		t.Fatalf("new application: %v", app)
	}
	// PAN check needs the legal name first.
	if st, o := s.do("POST", "/seller/application/pan", map[string]string{"pan": "ABCDE1234F"}); st != 422 || o["code"] != "LEGAL_NAME_REQUIRED" {
		t.Fatalf("pan before name: %d %v", st, o)
	}
	b := map[string]any{}
	for k, v := range business {
		b[k] = v
	}
	b["gstin"] = validGSTIN("27", "ABCDE1234F")
	s.must("PUT", "/seller/application", b, 200)

	// Not found PAN, then a good one.
	app = s.must("POST", "/seller/application/pan", map[string]string{"pan": "AAAAA0000A"}, 200)
	if app["panVerified"] != false || app["panCheck"].(map[string]any)["status"] != "FAILED" {
		t.Fatalf("failed pan: %v", app)
	}
	app = s.must("POST", "/seller/application/pan", map[string]string{"pan": "abcde1234f"}, 200)
	if app["panVerified"] != true || app["panLast4"] != "234F" {
		t.Fatalf("good pan: %v", app)
	}
	// Someone else's account, then the seller's own.
	app = s.must("POST", "/seller/application/bank", map[string]string{"account": "123456780000", "ifsc": "HDFC0001234"}, 200)
	if app["bankVerified"] != false || app["bankCheck"].(map[string]any)["status"] != "NAME_MISMATCH" {
		t.Fatalf("mismatch: %v", app)
	}
	app = s.must("POST", "/seller/application/bank", map[string]string{"account": "1234 5678 1234", "ifsc": "hdfc0001234"}, 200)
	if app["bankVerified"] != true || app["bankLast4"] != "1234" || len(app["missing"].([]any)) != 0 {
		t.Fatalf("good bank: %v", app)
	}

	if st, o := s.do("POST", "/seller/application/submit", map[string]bool{"acceptTerms": false}); st != 422 {
		t.Fatalf("submit without terms: %d %v", st, o)
	}
	app = s.must("POST", "/seller/application/submit", map[string]bool{"acceptTerms": true}, 200)
	if app["approval"] != "SUBMITTED" || app["editable"] != false {
		t.Fatalf("submitted: %v", app)
	}
	if st, o := s.do("PUT", "/seller/application", b); st != 409 || o["code"] != "APPLICATION_LOCKED" {
		t.Fatalf("edit after submit: %d %v", st, o)
	}

	// Admin reviews: the queue shows it, a vague rejection is refused, then reject.
	a := e.admin()
	q := a.must("GET", "/admin/approvals/sellers", nil, 200)["applications"].([]any)
	if len(q) != 1 || q[0].(map[string]any)["storeName"] != "Asha Electronics" {
		t.Fatalf("queue: %v", q)
	}
	id := q[0].(map[string]any)["accountId"].(string)
	d := a.must("GET", "/admin/approvals/sellers/"+id, nil, 200)
	if d["gstinMatchesPan"] != true || d["phone"] != "+919876500011" {
		t.Fatalf("detail: %v", d)
	}
	if st, _ := a.do("POST", "/admin/approvals/sellers/"+id+"/decision", map[string]string{"decision": "REJECT", "reason": "no"}); st != 400 {
		t.Fatalf("short reason: %d", st)
	}
	a.must("POST", "/admin/approvals/sellers/"+id+"/decision", map[string]string{"decision": "REJECT", "reason": "Pickup address must be a business address"}, 200)
	if sms := e.inbox.last("+919876500011", "approval_result"); !strings.Contains(sms.Body, "Pickup address must be") {
		t.Fatalf("rejection sms: %+v", sms)
	}

	// Seller fixes and resubmits; the admin approves; deciding twice is refused.
	app = s.must("GET", "/seller/application", nil, 200)
	if app["approval"] != "REJECTED" || app["reason"] != "Pickup address must be a business address" || app["editable"] != true {
		t.Fatalf("rejected view: %v", app)
	}
	s.must("PUT", "/seller/application", b, 200)
	s.must("POST", "/seller/application/submit", map[string]bool{"acceptTerms": true}, 200)
	a.must("POST", "/admin/approvals/sellers/"+id+"/decision", map[string]string{"decision": "APPROVE"}, 200)
	if st, o := a.do("POST", "/admin/approvals/sellers/"+id+"/decision", map[string]string{"decision": "APPROVE"}); st != 409 || o["code"] != "NOT_PENDING" {
		t.Fatalf("second decision: %d %v", st, o)
	}
	if me := s.must("GET", "/me", nil, 200); me["approval"] != "APPROVED" {
		t.Fatalf("me after approval: %v", me)
	}
}

func TestOnboardingGuards(t *testing.T) {
	e := setup(t)
	// Buyers cannot use seller routes.
	buyer := e.browser()
	buyer.signIn("9876500021", "BUYER")
	if st, _ := buyer.do("GET", "/seller/application", nil); st != 403 {
		t.Fatalf("buyer: %d", st)
	}
	// GSTIN issued to another PAN blocks submission.
	s := e.browser()
	s.signIn("9876500022", "SELLER")
	b := map[string]any{}
	for k, v := range business {
		b[k] = v
	}
	b["gstin"] = validGSTIN("27", "ZZZZZ9999Z")
	s.must("PUT", "/seller/application", b, 200)
	s.must("POST", "/seller/application/pan", map[string]string{"pan": "ABCDE1234F"}, 200)
	s.must("POST", "/seller/application/bank", map[string]string{"account": "123456781234", "ifsc": "HDFC0001234"}, 200)
	if st, o := s.do("POST", "/seller/application/submit", map[string]bool{"acceptTerms": true}); st != 422 || !strings.Contains(o["detail"].(string), "GSTIN") {
		t.Fatalf("gstin mismatch: %d %v", st, o)
	}
	// Changing the legal name clears both checks.
	b["gstin"] = validGSTIN("27", "ABCDE1234F")
	b["legalName"] = "Asha V Enterprises"
	app := s.must("PUT", "/seller/application", b, 200)
	if app["panVerified"] != false || app["bankVerified"] != false {
		t.Fatalf("checks kept after name change: %v", app)
	}
	// A second seller cannot take the same store name.
	other := e.browser()
	other.signIn("9876500023", "SELLER")
	if st, o := other.do("PUT", "/seller/application", map[string]any{"storeName": "asha electronics"}); st != 409 || o["code"] != "STORE_NAME_TAKEN" {
		t.Fatalf("store name: %d %v", st, o)
	}
	// An admin viewing as a seller cannot decide or submit.
	a := e.admin()
	a.must("POST", "/admin/role-switch", map[string]string{"role": "SELLER"}, 200)
	// The portal opens as an empty, read-only preview instead of failing.
	if st, app := a.do("GET", "/seller/application", nil); st != 200 || app["approval"] != "APPROVED" || app["editable"] != false {
		t.Fatalf("acting admin portal: %d %v", st, app)
	}
	var profiles int
	_ = e.auth.Pool.QueryRow(context.Background(), `SELECT count(*) FROM seller_profiles p JOIN accounts a ON a.id = p.account_id WHERE a.role = 'ADMIN'`).Scan(&profiles)
	if profiles != 0 {
		t.Fatal("the preview created a seller profile for the admin")
	}
	if st, _ := a.do("PUT", "/seller/application", b); st != 403 {
		t.Fatalf("acting admin edit: %d", st)
	}
	a.must("POST", "/admin/role-switch", map[string]any{"role": nil}, 200)
}

// validGSTIN builds a GSTIN with a correct check character for tests.
func validGSTIN(state, pan string) string {
	const chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	base := state + pan + "1Z"
	sum := 0
	for i := 0; i < 14; i++ {
		p := strings.IndexByte(chars, base[i]) * (1 + i%2)
		sum += p/36 + p%36
	}
	return base + string(chars[(36-sum%36)%36])
}
