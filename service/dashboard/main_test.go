package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------- fakes ----------

type fakeInemd struct {
	path  string
	query string
	user  string
	calls int
	code  int
	body  string
}

func newFakeInemd(t *testing.T, f *fakeInemd) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		f.path, f.query, f.user = r.URL.Path, r.URL.RawQuery, r.Header.Get("X-User-ID")
		code := f.code
		if code == 0 {
			code = 200
		}
		body := f.body
		if body == "" {
			body = `{"ok":true}`
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(up.Close)
	return up
}

// fakeGate stands in for inemgate: it knows one valid session token and one
// valid initData payload.
type fakeGate struct {
	calls  int
	verify string // last token it was asked to verify
}

const (
	goodToken = "good-session-token"
	goodInit  = "auth_date=fresh&user=%7B%22id%22%3A8629427424%7D&hash=signed"
)

func newFakeGate(t *testing.T, g *fakeGate) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.calls++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/verify":
			g.verify = r.URL.Query().Get("token")
			if g.verify != goodToken {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":"bad signature"}`)
				return
			}
			_, _ = io.WriteString(w, `{"user_id":7,"telegram_user_id":8629427424,"display_name":"Dani","expires_at":9999999999}`)
		case "/v1/session":
			raw, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(raw), "signed") {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":"initData signature does not match"}`)
				return
			}
			exp := time.Now().Add(time.Hour).Unix()
			_, _ = io.WriteString(w, `{"token":"`+goodToken+`","user_id":7,"telegram_user_id":8629427424,"display_name":"Dani","expires_at":`+jsonInt(exp)+`}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// dash wires the dashboard against both fakes. userID "1" is the break-glass
// inemd user; a verified session must win over it.
func dash(t *testing.T, f *fakeInemd, g *fakeGate) *server {
	t.Helper()
	return &server{
		upstream: newFakeInemd(t, f).URL,
		gate:     newFakeGate(t, g).URL,
		userID:   "1",
		authUser: "dani",
		client:   &http.Client{Timeout: 5 * time.Second},
	}
}

func req(t *testing.T, h http.Handler, method, path, cookie, body string) (int, http.Header, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rdr)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code, rec.Header(), rec.Body.String()
}

func get(t *testing.T, h http.Handler, path, cookie string) (int, string) {
	t.Helper()
	code, _, body := req(t, h, http.MethodGet, path, cookie, "")
	return code, body
}

// ---------- identity ----------

func TestDataEndpointsRequireIdentity(t *testing.T) {
	f, g := &fakeInemd{}, &fakeGate{}
	h := dash(t, f, g).handler()

	for _, path := range []string{"/api/pockets", "/api/summary", "/api/txns", "/api/notes", "/api/meals", "/api/day", "/api/target", "/api/pocket?id=1"} {
		code, body := get(t, h, path, "")
		if code != 401 || !strings.Contains(body, "not authenticated") {
			t.Fatalf("%s without a session: %d %s", path, code, body)
		}
	}
	if f.calls != 0 {
		t.Fatalf("an anonymous request reached inemd (%d calls)", f.calls)
	}

	// a forged/stale cookie is refused, and the gate is the one that decides
	if code, _ := get(t, h, "/api/pockets", "forged-token"); code != 401 {
		t.Fatalf("forged cookie: %d", code)
	}
	if g.verify != "forged-token" {
		t.Fatalf("the gate was not asked to verify, saw %q", g.verify)
	}
	if f.calls != 0 {
		t.Fatal("a forged cookie reached inemd")
	}
}

func TestSessionServesThatPersonsData(t *testing.T) {
	f, g := &fakeInemd{}, &fakeGate{}
	h := dash(t, f, g).handler()

	code, body := get(t, h, "/api/pockets", goodToken)
	if code != 200 {
		t.Fatalf("authenticated: %d %s", code, body)
	}
	if f.user != "7" {
		t.Fatalf("X-User-ID %q: a verified session must decide whose data is served, not the configured fallback", f.user)
	}
}

func TestGateDownFailsClosed(t *testing.T) {
	f := &fakeInemd{}
	s := dash(t, f, &fakeGate{})
	s.gate = "http://127.0.0.1:1" // nothing listens there
	h := s.handler()
	if code, _ := get(t, h, "/api/pockets", goodToken); code != 401 {
		t.Fatalf("gate unreachable should not let anyone in, got %d", code)
	}
	if f.calls != 0 {
		t.Fatal("data was served while the gate was unreachable")
	}
	// ...but the page itself still loads, so the user sees the login screen
	if code, _ := get(t, h, "/", ""); code != 200 {
		t.Fatalf("index: %d", code)
	}
}

func TestWhoami(t *testing.T) {
	f, g := &fakeInemd{}, &fakeGate{}
	h := dash(t, f, g).handler()

	code, body := get(t, h, "/api/whoami", "")
	if code != 200 || !strings.Contains(body, `"authenticated":false`) {
		t.Fatalf("anonymous whoami: %d %s", code, body)
	}
	code, body = get(t, h, "/api/whoami", goodToken)
	if code != 200 || !strings.Contains(body, `"authenticated":true`) || !strings.Contains(body, "Dani") {
		t.Fatalf("authenticated whoami: %d %s", code, body)
	}
}

// The dashboard offers tabs based on the member's scope, and it must learn that
// from inemd — never from anything the page sent.
func TestWhoamiCarriesMemberScope(t *testing.T) {
	f := &fakeInemd{body: `{"user_id":12,"display_name":"Pipit","scope":"finance"}`}
	h := dash(t, f, &fakeGate{}).handler()

	code, body := get(t, h, "/api/whoami", goodToken)
	if code != 200 || !strings.Contains(body, `"scope":"finance"`) || !strings.Contains(body, "Pipit") {
		t.Fatalf("whoami should carry the member's scope: %d %s", code, body)
	}
	if f.path != "/v1/me" || f.user != "7" {
		t.Fatalf("whoami should ask inemd who the caller is: path=%s user=%s", f.path, f.user)
	}
}

// ---------- login ----------

func TestAuthTelegram(t *testing.T) {
	f, g := &fakeInemd{}, &fakeGate{}
	h := dash(t, f, g).handler()

	code, hdr, body := req(t, h, http.MethodPost, "/auth/telegram", "", `{"init_data":"`+goodInit+`"}`)
	if code != 200 {
		t.Fatalf("login: %d %s", code, body)
	}
	cookies := hdr.Values("Set-Cookie")
	if len(cookies) != 1 {
		t.Fatalf("want one cookie, got %v", cookies)
	}
	c := cookies[0]
	for _, want := range []string{sessionCookie + "=" + goodToken, "HttpOnly", "Secure", "SameSite=Lax", "Path=/"} {
		if !strings.Contains(c, want) {
			t.Fatalf("cookie %q is missing %q", c, want)
		}
	}

	// a payload the gate rejects must not set a cookie, and the reason is shown
	code, hdr, body = req(t, h, http.MethodPost, "/auth/telegram", "", `{"init_data":"forged"}`)
	if code != 401 || !strings.Contains(body, "signature") {
		t.Fatalf("forged init_data: %d %s", code, body)
	}
	if len(hdr.Values("Set-Cookie")) != 0 {
		t.Fatal("a rejected login must not set a cookie")
	}

	if code, _, _ := req(t, h, http.MethodPost, "/auth/telegram", "", `{"init_data":""}`); code != 400 {
		t.Fatalf("empty init_data: %d", code)
	}
	if code, _, _ := req(t, h, http.MethodPost, "/auth/telegram", "", `not json`); code != 400 {
		t.Fatalf("bad json: %d", code)
	}
}

func TestBreakGlassPassword(t *testing.T) {
	f, g := &fakeInemd{}, &fakeGate{}
	s := dash(t, f, g)
	s.passwd = "s3cret"
	h := s.handler()

	// disabled by default: with no password configured the route does not exist
	plain := dash(t, &fakeInemd{}, &fakeGate{}).handler()
	if code, _, _ := req(t, plain, http.MethodPost, "/auth/password", "", `{"user":"dani","password":"s3cret"}`); code != 404 {
		t.Fatalf("password login should be off when unset: %d", code)
	}

	if code, _, _ := req(t, h, http.MethodPost, "/auth/password", "", `{"user":"dani","password":"nope"}`); code != 401 {
		t.Fatalf("wrong password: %d", code)
	}
	code, hdr, _ := req(t, h, http.MethodPost, "/auth/password", "", `{"user":"dani","password":"s3cret"}`)
	if code != 200 || len(hdr.Values("Set-Cookie")) != 1 {
		t.Fatalf("password login: %d %v", code, hdr.Values("Set-Cookie"))
	}

	// basic auth stays usable for scripts/emergencies
	r := httptest.NewRequest(http.MethodGet, "/api/pockets", nil)
	r.SetBasicAuth("dani", "s3cret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 200 || f.user != "1" {
		t.Fatalf("basic auth: %d (X-User-ID %q)", rec.Code, f.user)
	}
	code, body := get(t, h, "/api/whoami", "")
	if code != 200 || !strings.Contains(body, `"password_login":true`) {
		t.Fatalf("whoami should advertise password login: %s", body)
	}
}

func TestLogoutClearsTheCookie(t *testing.T) {
	f, g := &fakeInemd{}, &fakeGate{}
	h := dash(t, f, g).handler()
	code, hdr, _ := req(t, h, http.MethodGet, "/auth/logout", goodToken, "")
	if code != http.StatusSeeOther {
		t.Fatalf("logout: %d", code)
	}
	c := hdr.Get("Set-Cookie")
	if !strings.Contains(c, sessionCookie+"=") || !strings.Contains(c, "Max-Age=0") {
		t.Fatalf("logout cookie: %q", c)
	}
}

// ---------- passthrough ----------

func TestPassthroughPathsAndHeader(t *testing.T) {
	cases := []struct {
		request, wantPath, wantQueryPart string
	}{
		{"/api/pockets", "/v1/pockets", ""},
		{"/api/summary?period=today", "/v1/expense/summary", "from=" + time.Now().Format("2006-01-02")},
		{"/api/txns?period=month&limit=50", "/v1/transactions", "limit=50"},
		{"/api/notes", "/v1/notes", "limit=100"},
		{"/api/notes?tag=bills", "/v1/notes", "tag=bills"},
		{"/api/notes?q=wifi%20bill", "/v1/notes/search", "q=wifi%20bill"},
		{"/api/notes/12", "/v1/notes/12", ""},
		{"/api/household", "/v1/household", ""},
		{"/api/meals?day=2026-09-01", "/v1/meals", "day=2026-09-01"},
		{"/api/day", "/v1/nutrition/daily", "day=" + time.Now().Format("2006-01-02")},
		{"/api/target", "/v1/nutrition/target", "day=" + time.Now().Format("2006-01-02")},
	}
	for _, c := range cases {
		t.Run(c.request, func(t *testing.T) {
			f, g := &fakeInemd{}, &fakeGate{}
			h := dash(t, f, g).handler()
			code, body := get(t, h, c.request, goodToken)
			if code != 200 {
				t.Fatalf("status %d (%s)", code, body)
			}
			if f.path != c.wantPath {
				t.Fatalf("upstream path %q, want %q", f.path, c.wantPath)
			}
			if c.wantQueryPart != "" && !strings.Contains(f.query, c.wantQueryPart) {
				t.Fatalf("upstream query %q, want it to contain %q", f.query, c.wantQueryPart)
			}
			if f.user != "7" {
				t.Fatalf("X-User-ID %q, want 7", f.user)
			}
		})
	}
}

func TestReadOnlySurface(t *testing.T) {
	f, g := &fakeInemd{}, &fakeGate{}
	h := dash(t, f, g).handler()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		code, _, _ := req(t, h, method, "/api/pockets", goodToken, `{}`)
		if code != http.StatusMethodNotAllowed && code != http.StatusNotFound {
			t.Fatalf("%s /api/pockets should not be routed, got %d", method, code)
		}
		if f.calls != 0 {
			t.Fatalf("%s reached inemd — the dashboard must never write", method)
		}
	}
	if code, _ := get(t, h, "/api/nope", goodToken); code != 404 {
		t.Fatalf("unknown api path: %d", code)
	}
	if code, _ := get(t, h, "/seed", goodToken); code != 404 {
		t.Fatalf("unknown page: %d", code)
	}
}

func TestIndexIsServedAndOffline(t *testing.T) {
	f, g := &fakeInemd{}, &fakeGate{}
	h := dash(t, f, g).handler()
	code, body := get(t, h, "/", "")
	if code != 200 {
		t.Fatalf("index status %d", code)
	}
	for _, needle := range []string{"Mpok Inem", "/api/summary", "/api/notes", "/api/meals", "/api/whoami", "/auth/telegram", "/api/household", "read-only"} {
		if !strings.Contains(body, needle) {
			t.Fatalf("index is missing %q", needle)
		}
	}
	if strings.Contains(body, `src="http`) || strings.Contains(body, `href="http`) || strings.Contains(body, "cdn.") {
		t.Fatal("index must not depend on external assets (the signed payload arrives in the URL hash)")
	}
}

func TestUpstreamErrorsArePassedThrough(t *testing.T) {
	f := &fakeInemd{code: 409, body: `{"error":"pocket has entries"}`}
	g := &fakeGate{}
	h := dash(t, f, g).handler()
	code, body := get(t, h, "/api/pockets", goodToken)
	if code != 409 || !strings.Contains(body, "pocket has entries") {
		t.Fatalf("upstream error not passed through: %d %s", code, body)
	}

	down := dash(t, &fakeInemd{}, &fakeGate{})
	down.upstream = "http://127.0.0.1:1"
	code, body = get(t, down.handler(), "/api/pockets", goodToken)
	if code != http.StatusBadGateway {
		t.Fatalf("unreachable inemd: %d %s", code, body)
	}
	var msg map[string]string
	if err := json.Unmarshal([]byte(body), &msg); err != nil || msg["error"] == "" {
		t.Fatalf("bad gateway body: %s", body)
	}
}

func TestPocketDetailMergesPocketLedgerAndTransfers(t *testing.T) {
	var got []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path+"?"+r.URL.RawQuery)
		if r.Header.Get("X-User-ID") != "7" {
			t.Errorf("missing X-User-ID on %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/pockets/3":
			_, _ = io.WriteString(w, `{"id":3,"name":"Cash","type":"cash","balance_idr":13000}`)
		case "/v1/transactions":
			_, _ = io.WriteString(w, `[
				{"id":2,"direction":"out","amount_idr":25000,"category":"food","note":"kopi","created_at":"2026-09-20T09:05:00+07:00"},
				{"id":1,"direction":"in","amount_idr":5000,"category":"refund","note":"","created_at":"2026-09-19T08:00:00+07:00"}]`)
		case "/v1/transfers":
			_, _ = io.WriteString(w, `[
				{"id":5,"from_pocket_id":3,"from":"Cash","to":"Savings","amount_idr":50000,"note":"save","created_at":"2026-09-20T10:00:00+07:00"},
				{"id":6,"from_pocket_id":2,"from":"BRImo","to":"Cash","amount_idr":100000,"note":"","created_at":"2026-09-18T07:00:00+07:00"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	g := &fakeGate{}
	s := &server{upstream: up.URL, gate: newFakeGate(t, g).URL, userID: "1", client: up.Client()}

	code, body := get(t, s.handler(), "/api/pocket?id=3&period=month", goodToken)
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if raw["pocket"].(map[string]any)["name"] != "Cash" {
		t.Fatalf("pocket passthrough: %s", body)
	}
	if int(raw["user_id"].(float64)) != 7 {
		t.Fatalf("composed view should report the session's user: %v", raw["user_id"])
	}
	totals := raw["totals"].(map[string]any)
	if totals["out_idr"].(float64) != 25000 || totals["in_idr"].(float64) != 5000 ||
		totals["transfer_out_idr"].(float64) != 50000 || totals["transfer_in_idr"].(float64) != 100000 {
		t.Fatalf("totals: %v", totals)
	}
	moves := raw["movements"].([]any)
	if len(moves) != 4 {
		t.Fatalf("movements: %v", moves)
	}
	first := moves[0].(map[string]any)
	if first["kind"] != "transfer" || first["direction"] != "out" || first["label"] != "→ Savings · save" {
		t.Fatalf("outgoing transfer row: %v", first)
	}
	second := moves[1].(map[string]any)
	if second["kind"] != "txn" || second["direction"] != "out" || second["label"] != "kopi" {
		t.Fatalf("txn row: %v", second)
	}
	last := moves[3].(map[string]any)
	if last["direction"] != "in" || last["label"] != "← BRImo" {
		t.Fatalf("incoming transfer row: %v", last)
	}
	if moves[2].(map[string]any)["label"] != "refund" {
		t.Fatalf("category fallback: %v", moves[2])
	}

	wantQueries := []string{
		"/v1/pockets/3?",
		"/v1/transactions?month=" + time.Now().Format("2006-01") + "&pocket_id=3&limit=500",
		"/v1/transfers?month=" + time.Now().Format("2006-01") + "&pocket_id=3&limit=500",
	}
	if len(got) != len(wantQueries) {
		t.Fatalf("upstream calls: %v", got)
	}
	for i, want := range wantQueries {
		if got[i] != want {
			t.Fatalf("call %d = %q, want %q", i, got[i], want)
		}
	}
}

func TestPocketDetailGuards(t *testing.T) {
	f, g := &fakeInemd{}, &fakeGate{}
	h := dash(t, f, g).handler()
	if code, _ := get(t, h, "/api/pocket", goodToken); code != 400 {
		t.Fatalf("missing id: %d", code)
	}
	if code, _ := get(t, h, "/api/pocket?id=abc", goodToken); code != 400 {
		t.Fatalf("bad id: %d", code)
	}
	if code, _ := get(t, h, "/api/pocket?id=1&period=nonsense", goodToken); code != 400 {
		t.Fatalf("bad period: %d", code)
	}
	down := dash(t, &fakeInemd{}, &fakeGate{})
	down.upstream = "http://127.0.0.1:1"
	if code, _ := get(t, down.handler(), "/api/pocket?id=1", goodToken); code != http.StatusBadGateway {
		t.Fatalf("unreachable inemd: %d", code)
	}
}

// ---------- pure helpers ----------

func TestPeriodQuery(t *testing.T) {
	today := time.Now()
	iso := func(d time.Time) string { return d.Format("2006-01-02") }
	weekStart := today.AddDate(0, 0, -((int(today.Weekday()) + 6) % 7))

	cases := []struct {
		period, want string
		bad          bool
	}{
		{"", "?month=" + today.Format("2006-01"), false},
		{"month", "?month=" + today.Format("2006-01"), false},
		{today.Format("2006-01"), "?month=" + today.Format("2006-01"), false},
		{"today", "?from=" + iso(today) + "&to=" + iso(today), false},
		{"yesterday", "?from=" + iso(today.AddDate(0, 0, -1)) + "&to=" + iso(today.AddDate(0, 0, -1)), false},
		{"week", "?from=" + iso(weekStart) + "&to=" + iso(today), false},
		{"last-week", "?from=" + iso(weekStart.AddDate(0, 0, -7)) + "&to=" + iso(weekStart.AddDate(0, 0, -1)), false},
		{"last-month", "?month=" + time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, today.Location()).AddDate(0, 0, -1).Format("2006-01"), false},
		{"2026-01-05..2026-02-03", "?from=2026-01-05&to=2026-02-03", false},
		{"2026-13-05..2026-02-03", "", true},
		{"nonsense", "", true},
		{"2026-1", "", true},
	}
	for _, c := range cases {
		t.Run(c.period, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/summary?period="+c.period, nil)
			got, err := periodQuery(r)
			if c.bad {
				if err == nil {
					t.Fatalf("want an error, got %q", got)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("periodQuery(%q) = %q (%v), want %q", c.period, got, err, c.want)
			}
		})
	}
}

func TestHelpers(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/meals?day=2026-09-01", nil)
	if got := dayOr(r, "2026-01-01"); got != "2026-09-01" {
		t.Fatalf("dayOr: %q", got)
	}
	r = httptest.NewRequest(http.MethodGet, "/api/meals?day=01-09-2026", nil)
	if got := dayOr(r, "2026-01-01"); got != "2026-01-01" {
		t.Fatalf("dayOr should fall back: %q", got)
	}
	for _, c := range []struct{ in, want string }{{"", "100"}, {"50", "50"}, {"0", "100"}, {"abc", "100"}, {"501", "500"}} {
		r = httptest.NewRequest(http.MethodGet, "/api/txns?limit="+c.in, nil)
		if got := limitOr(r, "100", "500"); got != c.want {
			t.Fatalf("limitOr(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := urlQueryEscape("wifi bill & home"); got != "wifi%20bill%20%26%20home" {
		t.Fatalf("urlQueryEscape: %q", got)
	}
	monday := monday(time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)) // a Sunday
	if monday.Format("2006-01-02") != "2026-09-14" || monday.Hour() != 0 {
		t.Fatalf("monday(): %v", monday)
	}
	for _, c := range []struct{ path, want string }{
		{"/", "/"}, {"/auth/telegram", "/"}, {"/inem/", "/inem/"}, {"/inem/auth/telegram", "/inem/"}, {"/inem", "/inem/"},
	} {
		r = httptest.NewRequest(http.MethodGet, c.path, nil)
		if got := cookiePath(r); got != c.want {
			t.Fatalf("cookiePath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}
