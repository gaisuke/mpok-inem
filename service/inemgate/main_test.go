package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testBotToken = "123456:AAHfake-bot-token-for-tests-only"
	testSecret   = "0123456789abcdef0123456789abcdef-test-session-secret"
)

// signInitData mirrors Telegram's documented snippet:
//
//	secret = hmac.new(b"WebAppData", bot_token, sha256).digest()
//	hash   = hmac.new(secret, data_check_string, sha256).hexdigest()
//
// Written out separately from the implementation so the test is an independent
// statement of the algorithm, not a call into it. (A live initData should still
// be checked once end to end — Telegram is the only oracle for its own HMAC.)
func signInitData(fields map[string]string, botToken string) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		if k != "hash" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+fields[k])
	}
	dataCheck := strings.Join(parts, "\n")

	mac := hmac.New(sha256.New, []byte("WebAppData"))
	mac.Write([]byte(botToken))
	secret := mac.Sum(nil)
	mac = hmac.New(sha256.New, secret)
	mac.Write([]byte(dataCheck))
	fields["hash"] = hex.EncodeToString(mac.Sum(nil))

	vals := url.Values{}
	for k, v := range fields {
		vals.Set(k, v)
	}
	return vals.Encode()
}

func validInitData(t *testing.T, telegramID int64, authDate time.Time) string {
	t.Helper()
	return signInitData(map[string]string{
		"auth_date":     fmt.Sprint(authDate.Unix()),
		"query_id":      "AAHdF6IQAAAAAN0Xoha8",
		"chat_instance": "8428209589180549439",
		"chat_type":     "private",
		"user":          fmt.Sprintf(`{"id":%d,"first_name":"Dani","last_name":"Munif","username":"gaisuke","language_code":"id"}`, telegramID),
	}, testBotToken)
}

func TestVerifyInitData(t *testing.T) {
	now := time.Now()
	id, name, err := verifyInitData(validInitData(t, 8629427424, now), testBotToken, 5*time.Minute, now)
	if err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}
	if id != 8629427424 || name != "Dani Munif" {
		t.Fatalf("got id=%d name=%q", id, name)
	}
}

func TestVerifyInitDataRejections(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name     string
		initData func() string
		botToken string
		at       time.Time
		wantErr  error
	}{
		{
			name:     "tampered user id (signature no longer matches)",
			initData: func() string { return strings.Replace(validInitData(t, 1, now), "id%22%3A1", "id%22%3A2", 1) },
			botToken: testBotToken, at: now, wantErr: errBadHash,
		},
		{
			name: "signed with a different bot token",
			initData: func() string {
				return signInitData(map[string]string{"auth_date": fmt.Sprint(now.Unix()), "user": `{"id":5}`}, "999:other-bot")
			},
			botToken: testBotToken, at: now, wantErr: errBadHash,
		},
		{
			name:     "no hash at all",
			initData: func() string { return "auth_date=" + fmt.Sprint(now.Unix()) + "&user=%7B%22id%22%3A5%7D" },
			botToken: testBotToken, at: now, wantErr: errNoHash,
		},
		{
			name:     "no auth_date",
			initData: func() string { return signInitData(map[string]string{"user": `{"id":5}`}, testBotToken) },
			botToken: testBotToken, at: now, wantErr: errNoAuthDate,
		},
		{
			name: "user is not json",
			initData: func() string {
				return signInitData(map[string]string{"auth_date": fmt.Sprint(now.Unix()), "user": "nope"}, testBotToken)
			},
			botToken: testBotToken, at: now, wantErr: errNoUser,
		},
		{
			name:     "stale: replayed an hour later",
			initData: func() string { return validInitData(t, 7, now.Add(-time.Hour)) },
			botToken: testBotToken, at: now, wantErr: errStale,
		},
		{
			name:     "clock skew the other way (auth_date in the future)",
			initData: func() string { return validInitData(t, 7, now.Add(time.Hour)) },
			botToken: testBotToken, at: now, wantErr: errStale,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := verifyInitData(c.initData(), c.botToken, 5*time.Minute, c.at)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("got %v, want %v", err, c.wantErr)
			}
		})
	}
}

func TestSessionRoundTrip(t *testing.T) {
	now := time.Now()
	s := session{UserID: 3, TelegramUserID: 8629427424, ExpiresAt: now.Add(time.Hour).Unix(), Name: "Dani"}
	token, err := mintSession(s, testSecret)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	back, err := parseSession(token, testSecret, now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if back.UserID != 3 || back.TelegramUserID != 8629427424 {
		t.Fatalf("round trip: %+v", back)
	}

	if _, err := parseSession(token, "another-secret-but-also-long-enough-x", now); err == nil {
		t.Fatal("a token signed with another secret must not verify")
	}
	if _, err := parseSession(token, testSecret, now.Add(2*time.Hour)); err == nil {
		t.Fatal("expired token must not verify")
	}
	tampered := strings.Replace(token, token[:4], token[:4][:3]+"A", 1)
	if _, err := parseSession(tampered, testSecret, now); err == nil {
		t.Fatal("tampered token must not verify")
	}
	for _, bad := range []string{"", "nodot", "a.b", ".", "payload."} {
		if _, err := parseSession(bad, testSecret, now); err == nil {
			t.Fatalf("malformed token %q accepted", bad)
		}
	}
}

// ---------- http surface ----------

func fakeRoster(t *testing.T, hits *int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(hits, 1)
		if r.Header.Get("X-User-ID") == "" {
			t.Error("roster call without X-User-ID")
		}
		if r.URL.Path != "/v1/admin/users" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":1,"telegram_user_id":8629427424,"display_name":"Dani"},
			{"id":2,"telegram_user_id":555000111,"display_name":"Istri"}]`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestGate(t *testing.T, inemBase string) *gate {
	t.Helper()
	return &gate{
		botToken: testBotToken, secret: testSecret, inemBase: inemBase,
		adminUser: "1", ttl: time.Hour, maxSkew: 5 * time.Minute,
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

func post(t *testing.T, h http.Handler, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

func TestSessionEndpointEndToEnd(t *testing.T) {
	var hits int64
	g := newTestGate(t, fakeRoster(t, &hits).URL)
	h := g.handler()

	if code, _ := get(t, h, "/healthz"); code != 200 {
		t.Fatalf("healthz: %d", code)
	}

	body, _ := json.Marshal(map[string]string{"init_data": validInitData(t, 8629427424, time.Now())})
	code, out := post(t, h, "/v1/session", string(body))
	if code != 200 {
		t.Fatalf("session: %d %v", code, out)
	}
	if int(out["user_id"].(float64)) != 1 || out["display_name"] != "Dani" {
		t.Fatalf("session body: %v", out)
	}
	token, _ := out["token"].(string)
	if token == "" {
		t.Fatal("no token minted")
	}

	// the dashboard verifies the cookie through this endpoint
	code, out = getJSON(t, h, "/v1/verify?token="+url.QueryEscape(token))
	if code != 200 || int(out["user_id"].(float64)) != 1 {
		t.Fatalf("verify: %d %v", code, out)
	}

	// a second lookup is served from the roster cache
	code, _ = post(t, h, "/v1/session", string(body))
	if code != 200 {
		t.Fatalf("second session: %d", code)
	}
	if n := atomic.LoadInt64(&hits); n != 1 {
		t.Fatalf("roster should be cached, got %d fetches", n)
	}
}

func TestSessionEndpointRejections(t *testing.T) {
	var hits int64
	g := newTestGate(t, fakeRoster(t, &hits).URL)
	h := g.handler()

	code, out := post(t, h, "/v1/session", `{"init_data":""}`)
	if code != 400 {
		t.Fatalf("empty init_data: %d %v", code, out)
	}
	code, _ = post(t, h, "/v1/session", `not json`)
	if code != 400 {
		t.Fatalf("bad json: %d", code)
	}

	tampered := strings.Replace(validInitData(t, 8629427424, time.Now()), "8629427424", "1", 1)
	body, _ := json.Marshal(map[string]string{"init_data": tampered})
	if code, out := post(t, h, "/v1/session", string(body)); code != 401 {
		t.Fatalf("forged signature should be 401, got %d %v", code, out)
	}

	stale, _ := json.Marshal(map[string]string{"init_data": validInitData(t, 8629427424, time.Now().Add(-time.Hour))})
	if code, _ := post(t, h, "/v1/session", string(stale)); code != 401 {
		t.Fatalf("stale init_data should be 401, got %d", code)
	}

	// signed by Telegram, but this person is not a household member yet
	stranger, _ := json.Marshal(map[string]string{"init_data": validInitData(t, 42424242, time.Now())})
	code, out = post(t, h, "/v1/session", string(stranger))
	if code != 403 || !strings.Contains(fmt.Sprint(out["error"]), "not a registered member") {
		t.Fatalf("unknown member: %d %v", code, out)
	}

	if code, _ := getJSON(t, h, "/v1/verify?token=garbage"); code != 401 {
		t.Fatalf("garbage token: %d", code)
	}
	if code, _ := getJSON(t, h, "/v1/verify"); code != 401 {
		t.Fatalf("missing token: %d", code)
	}

	// reads only, and writing is not part of this surface
	req := httptest.NewRequest(http.MethodPost, "/v1/verify?token=x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/verify: %d", rec.Code)
	}
}

func TestLookupFailsClosedWhenRosterUnreachable(t *testing.T) {
	var hits int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer up.Close()
	_ = hits
	g := newTestGate(t, up.URL)
	body, _ := json.Marshal(map[string]string{"init_data": validInitData(t, 8629427424, time.Now())})
	if code, _ := post(t, g.handler(), "/v1/session", string(body)); code != 403 {
		t.Fatalf("a broken roster must not let anyone in, got %d", code)
	}
}

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

func getJSON(t *testing.T, h http.Handler, path string) (int, map[string]any) {
	t.Helper()
	code, body := get(t, h, path)
	var out map[string]any
	if body != "" {
		_ = json.Unmarshal([]byte(body), &out)
	}
	return code, out
}
