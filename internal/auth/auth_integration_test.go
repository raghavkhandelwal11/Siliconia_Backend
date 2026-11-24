package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/config"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/auth"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/health"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/crypto"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/db"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/migrate"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/platform/ratelimit"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/provider/messaging"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/server"
	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// inbox captures OTP messages instead of sending them.
type inbox struct {
	mu   sync.Mutex
	last map[string]string
}

var codeRE = regexp.MustCompile(`\b\d{6}\b`)

func (b *inbox) Send(_ context.Context, m messaging.Message) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.last[m.To] = codeRE.FindString(m.Body)
	return "test", nil
}

func (b *inbox) code(phone string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.last[phone]
}

type env struct {
	t     *testing.T
	srv   *httptest.Server
	svc   *auth.Service
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
	if _, err := pool.Exec(ctx, `TRUNCATE accounts, sessions, otp_challenges, audit_log CASCADE`); err != nil {
		t.Fatal(err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	signer, _ := auth.NewSigner(config.DevKey("auth-signing"), auth.AccessTTL)
	box, _ := crypto.NewBox(config.DevKey("encryption-master"))
	in := &inbox{last: map[string]string{}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := &auth.Service{
		Pool: pool, Q: store.New(pool), Redis: rdb, Limiter: ratelimit.New(rdb), Messages: in,
		Signer: signer, Box: box, Pepper: []byte("test-pepper"), Log: log,
	}
	h := server.New(server.Deps{
		Log: log, Health: &health.Handler{}, AuthService: svc, Auth: auth.NewHandler(svc, nil, false),
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, svc: svc, inbox: in, mr: mr}
}

// client is one browser: a cookie jar plus the CSRF header the web app sends.
type client struct {
	e   *env
	jar *cookiejar.Jar
	hc  *http.Client
}

func (e *env) browser() *client {
	jar, _ := cookiejar.New(nil)
	return &client{e: e, jar: jar, hc: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (c *client) cookie(name, path string) string {
	u, _ := url.Parse(c.e.srv.URL + path)
	for _, ck := range c.jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

func (c *client) do(method, path string, body any, csrf bool) (int, map[string]any) {
	c.e.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.e.srv.URL+"/api/v1"+path, r)
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		req.Header.Set(auth.HeaderCSRF, c.cookie(auth.CookieCSRF, "/"))
	}
	res, err := c.hc.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer res.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func (c *client) signIn(phone, role string) map[string]any {
	c.e.t.Helper()
	st, out := c.do("POST", "/auth/otp/send", map[string]string{"phone": phone, "role": role}, false)
	if st != 200 {
		c.e.t.Fatalf("send: %d %v", st, out)
	}
	normalized, _ := auth.NormalizeIndianPhone(phone)
	st, out = c.do("POST", "/auth/otp/verify", map[string]string{"challengeId": out["challengeId"].(string), "code": c.e.inbox.code(normalized)}, false)
	if st != 200 {
		c.e.t.Fatalf("verify: %d %v", st, out)
	}
	return out
}

func TestOTPSignInCreatesOneAccountPerRole(t *testing.T) {
	e := setup(t)
	b := e.browser()
	out := b.signIn("98765 43210", "BUYER")
	if out["isNew"] != true {
		t.Fatalf("first sign-in should create the account: %v", out)
	}
	st, me := b.do("GET", "/me", nil, false)
	if st != 200 || me["role"] != "BUYER" || me["approval"] != "APPROVED" {
		t.Fatalf("me: %d %v", st, me)
	}

	// Same number, same role → same account; different role → a separate account (FR-ROLE-02).
	e.mr.FastForward(31 * time.Second)
	if again := e.browser().signIn("9876543210", "BUYER"); again["isNew"] != false {
		t.Fatalf("second sign-in should reuse the account: %v", again)
	}
	e.mr.FastForward(31 * time.Second)
	seller := e.browser().signIn("9876543210", "SELLER")
	acc := seller["account"].(map[string]any)
	if seller["isNew"] != true || acc["approval"] != "DRAFT" {
		t.Fatalf("seller account: %v", seller)
	}
}

func TestOTPRejectsWrongCodeAndLocksAfterFiveTries(t *testing.T) {
	e := setup(t)
	b := e.browser()
	_, out := b.do("POST", "/auth/otp/send", map[string]string{"phone": "9123456780", "role": "BUYER"}, false)
	id := out["challengeId"].(string)
	right := e.inbox.code("+919123456780")
	wrong := "000000"
	if right == wrong {
		wrong = "111111"
	}
	for i := 0; i < auth.OTPMaxAttempts; i++ {
		st, o := b.do("POST", "/auth/otp/verify", map[string]string{"challengeId": id, "code": wrong}, false)
		// The last allowed attempt already reports that the code is used up.
		want := "AUTH_OTP_INVALID"
		if i == auth.OTPMaxAttempts-1 {
			want = "AUTH_OTP_EXPIRED"
		}
		if st != 401 || o["code"] != want {
			t.Fatalf("attempt %d: %d %v", i, st, o)
		}
	}
	if st, o := b.do("POST", "/auth/otp/verify", map[string]string{"challengeId": id, "code": right}, false); st == 200 {
		t.Fatalf("right code must not work after 5 wrong tries: %v", o)
	}
}

func TestOTPResendCooldown(t *testing.T) {
	e := setup(t)
	b := e.browser()
	body := map[string]string{"phone": "9000000001", "role": "BUYER"}
	if st, _ := b.do("POST", "/auth/otp/send", body, false); st != 200 {
		t.Fatal(st)
	}
	if st, o := b.do("POST", "/auth/otp/send", body, false); st != 429 {
		t.Fatalf("resend within 30 s should be 429: %d %v", st, o)
	}
}

func TestCSRFIsRequiredOnceSignedIn(t *testing.T) {
	e := setup(t)
	b := e.browser()
	b.signIn("9000000002", "BUYER")
	if st, o := b.do("PATCH", "/me", map[string]string{"name": "Asha"}, false); st != 403 || o["code"] != "CSRF_FAILED" {
		t.Fatalf("without header: %d %v", st, o)
	}
	if st, o := b.do("PATCH", "/me", map[string]string{"name": "Asha"}, true); st != 200 || o["name"] != "Asha" {
		t.Fatalf("with header: %d %v", st, o)
	}
}

func TestRefreshRotatesAndReuseRevokesFamily(t *testing.T) {
	e := setup(t)
	b := e.browser()
	b.signIn("9000000003", "BUYER")
	stolen := b.cookie(auth.CookieRefresh, "/api/v1/auth")

	if st, o := b.do("POST", "/auth/refresh", nil, true); st != 200 {
		t.Fatalf("refresh: %d %v", st, o)
	}
	if b.cookie(auth.CookieRefresh, "/api/v1/auth") == stolen {
		t.Fatal("refresh token must rotate")
	}

	// An attacker replays the old refresh token: the whole family is revoked.
	thief := e.browser()
	u, _ := url.Parse(e.srv.URL + "/api/v1/auth")
	thief.jar.SetCookies(u, []*http.Cookie{{Name: auth.CookieRefresh, Value: stolen, Path: "/api/v1/auth"}, {Name: auth.CookieCSRF, Value: "x", Path: "/"}})
	root, _ := url.Parse(e.srv.URL + "/")
	thief.jar.SetCookies(root, []*http.Cookie{{Name: auth.CookieCSRF, Value: "x", Path: "/"}})
	if st, _ := thief.do("POST", "/auth/refresh", nil, true); st != 401 {
		t.Fatalf("reused token should be 401, got %d", st)
	}
	if st, _ := b.do("POST", "/auth/refresh", nil, true); st != 401 {
		t.Fatalf("victim's rotated token must be revoked too, got %d", st)
	}
	if st, _ := b.do("GET", "/me", nil, false); st != 401 {
		t.Fatalf("access token must be denied after family revocation, got %d", st)
	}
}

func TestLogoutEndsSession(t *testing.T) {
	e := setup(t)
	b := e.browser()
	b.signIn("9000000004", "BUYER")
	if st, _ := b.do("POST", "/auth/logout", nil, true); st != 204 {
		t.Fatal(st)
	}
	if st, _ := b.do("GET", "/me", nil, false); st != 401 {
		t.Fatalf("after logout: %d", st)
	}
}

func TestAtMostThreeAdmins(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	for _, p := range []string{"9100000001", "9100000002", "9100000003"} {
		if _, err := e.svc.CreateAdmin(ctx, p, "Admin"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.svc.CreateAdmin(ctx, "9100000004", "Admin"); err != auth.ErrAdminLimit {
		t.Fatalf("fourth admin: %v", err)
	}
}

func TestAdminSignUpIsBlocked(t *testing.T) {
	e := setup(t)
	b := e.browser()
	_, out := b.do("POST", "/auth/otp/send", map[string]string{"phone": "9200000001", "role": "ADMIN"}, false)
	st, o := b.do("POST", "/auth/otp/verify", map[string]string{"challengeId": out["challengeId"].(string), "code": e.inbox.code("+919200000001")}, false)
	if st != 403 || o["code"] != "ROLE_NOT_ALLOWED" {
		t.Fatalf("admin self sign-up: %d %v", st, o)
	}
}

func TestAdminTOTPAndRoleSwitch(t *testing.T) {
	e := setup(t)
	if _, err := e.svc.CreateAdmin(context.Background(), "9300000001", "Root"); err != nil {
		t.Fatal(err)
	}
	b := e.browser()
	b.signIn("9300000001", "ADMIN")

	// Everything except TOTP is blocked until the authenticator code is entered.
	if st, o := b.do("GET", "/me", nil, false); st != 403 || o["code"] != "MFA_REQUIRED" {
		t.Fatalf("me before MFA: %d %v", st, o)
	}
	if st, o := b.do("POST", "/admin/role-switch", map[string]string{"role": "SELLER"}, true); st != 403 {
		t.Fatalf("switch before MFA: %d %v", st, o)
	}
	st, setup := b.do("POST", "/auth/totp/setup", nil, true)
	if st != 200 {
		t.Fatalf("setup: %d %v", st, setup)
	}
	if st, _ := b.do("POST", "/auth/totp/verify", map[string]string{"code": "000000x"}, true); st != 401 {
		t.Fatalf("bad totp: %d", st)
	}
	code, _ := auth.TOTPCode(setup["secret"].(string), time.Now())
	if st, o := b.do("POST", "/auth/totp/verify", map[string]string{"code": code}, true); st != 204 {
		t.Fatalf("verify: %d %v", st, o)
	}
	if st, _ := b.do("POST", "/auth/totp/setup", nil, true); st != 409 {
		t.Fatalf("second setup should conflict: %d", st)
	}

	st, sw := b.do("POST", "/admin/role-switch", map[string]string{"role": "SELLER"}, true)
	if st != 200 || sw["actingRole"] != "SELLER" {
		t.Fatalf("switch: %d %v", st, sw)
	}
	if _, me := b.do("GET", "/me", nil, false); me["actingRole"] != "SELLER" || me["role"] != "ADMIN" {
		t.Fatalf("me after switch: %v", me)
	}
	var n int
	if err := e.svc.Pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = 'admin.role_switch'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit rows: %d %v", n, err)
	}
	if st, sw := b.do("POST", "/admin/role-switch", map[string]any{"role": nil}, true); st != 200 || sw["actingRole"] != nil && sw["actingRole"] != "" {
		t.Fatalf("switch back: %d %v", st, sw)
	}
}

func TestNonAdminCannotSwitchRole(t *testing.T) {
	e := setup(t)
	b := e.browser()
	b.signIn("9000000005", "BUYER")
	if st, _ := b.do("POST", "/admin/role-switch", map[string]string{"role": "SELLER"}, true); st != 403 {
		t.Fatalf("buyer switch: %d", st)
	}
}
