// inemgate — the only component that holds the Telegram bot token.
//
// A Telegram Mini App cannot prove who opened it by sending a telegram id; the
// proof is the HMAC Telegram puts in `initData`, and verifying that HMAC needs
// the bot token. The bot token is the crown jewel (it can read every bot update
// and impersonate the bot), so it must not live in the internet-facing
// dashboard: inemgate binds 127.0.0.1 only, is never proxied by nginx, verifies
// initData, maps the telegram id to an inemd user, and mints a signed session
// token. inemdash then holds no secret at all — it just asks inemgate to verify
// a token.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	errNoHash     = errors.New("initData has no hash")
	errBadHash    = errors.New("initData signature does not match")
	errStale      = errors.New("initData is stale")
	errNoUser     = errors.New("initData has no valid user")
	errNoAuthDate = errors.New("initData has no auth_date")
)

// verifyInitData implements Telegram's Mini App check:
//
//	secret = HMAC_SHA256(key="WebAppData", message=bot_token)
//	hash   = hex(HMAC_SHA256(key=secret, message=data_check_string))
//
// where data_check_string is every field except `hash`, sorted by key, joined
// as "key=value" with newlines — the same algorithm as Telegram's documented
// Python snippet (hmac.new(b"WebAppData", bot_token, sha256)).
func verifyInitData(initData, botToken string, maxSkew time.Duration, now time.Time) (telegramID int64, displayName string, err error) {
	q, err := url.ParseQuery(initData)
	if err != nil {
		return 0, "", fmt.Errorf("initData is not a query string: %w", err)
	}
	got := q.Get("hash")
	if got == "" {
		return 0, "", errNoHash
	}
	rawAuthDate := q.Get("auth_date")
	if rawAuthDate == "" {
		return 0, "", errNoAuthDate
	}
	authDate, err := strconv.ParseInt(rawAuthDate, 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("auth_date is not a unix timestamp: %w", err)
	}
	// A signature is forever; freshness is what stops a leaked initData being
	// replayed months later.
	age := now.Sub(time.Unix(authDate, 0))
	if age > maxSkew || age < -maxSkew {
		return 0, "", fmt.Errorf("%w (age %s)", errStale, age.Round(time.Second))
	}

	keys := make([]string, 0, len(q))
	for k := range q {
		if k != "hash" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+q.Get(k))
	}
	dataCheck := strings.Join(parts, "\n")

	mac := hmac.New(sha256.New, []byte("WebAppData"))
	mac.Write([]byte(botToken))
	secret := mac.Sum(nil)
	mac = hmac.New(sha256.New, secret)
	mac.Write([]byte(dataCheck))
	want := hex.EncodeToString(mac.Sum(nil))

	if subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
		return 0, "", errBadHash
	}

	var user struct {
		ID        int64  `json:"id"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
		Username  string `json:"username"`
	}
	if err := json.Unmarshal([]byte(q.Get("user")), &user); err != nil || user.ID == 0 {
		return 0, "", errNoUser
	}
	name := strings.TrimSpace(user.FirstName + " " + user.LastName)
	if name == "" {
		name = user.Username
	}
	return user.ID, name, nil
}

// ---------- sessions ----------

type session struct {
	UserID         int    `json:"user_id"`        // inemd user
	TelegramUserID int64  `json:"tg_id"`          // telegram user
	ExpiresAt      int64  `json:"exp"`            // unix seconds
	Name           string `json:"name,omitempty"` // telegram display name, for the UI
}

func sign(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func mintSession(s session, secret string) (string, error) {
	body, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(body)
	return encoded + "." + sign([]byte(encoded), secret), nil
}

func parseSession(token, secret string, now time.Time) (session, error) {
	var s session
	encoded, sig, ok := strings.Cut(token, ".")
	if !ok {
		return s, errors.New("malformed token")
	}
	if subtle.ConstantTimeCompare([]byte(sign([]byte(encoded), secret)), []byte(sig)) != 1 {
		return s, errors.New("bad signature")
	}
	body, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return s, errors.New("malformed payload")
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return s, errors.New("malformed payload")
	}
	if s.ExpiresAt <= now.Unix() {
		return s, errors.New("session expired")
	}
	if s.UserID == 0 {
		return s, errors.New("session has no user")
	}
	return s, nil
}

// ---------- service ----------

type gate struct {
	botToken  string
	secret    string
	inemBase  string
	adminUser string
	ttl       time.Duration
	maxSkew   time.Duration
	client    *http.Client

	mu       sync.Mutex
	roster   map[int64]member
	cachedAt time.Time
}

type member struct {
	UserID         int    `json:"id"`
	TelegramUserID int64  `json:"telegram_user_id"`
	DisplayName    string `json:"display_name"`
}

const rosterTTL = 60 * time.Second

// lookup resolves a telegram id to an inemd member via the roster endpoint.
// Unknown ids are refused: an account must exist before it can log in.
func (g *gate) lookup(telegramID int64) (member, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Since(g.cachedAt) > rosterTTL || g.roster == nil {
		req, err := http.NewRequest(http.MethodGet, g.inemBase+"/v1/admin/users", nil)
		if err != nil {
			return member{}, false
		}
		req.Header.Set("X-User-ID", g.adminUser)
		resp, err := g.client.Do(req)
		if err != nil {
			return member{}, false
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil || resp.StatusCode != http.StatusOK {
			return member{}, false
		}
		var rows []member
		if err := json.Unmarshal(raw, &rows); err != nil {
			return member{}, false
		}
		g.roster = make(map[int64]member, len(rows))
		for _, m := range rows {
			g.roster[m.TelegramUserID] = m
		}
		g.cachedAt = time.Now()
	}
	m, ok := g.roster[telegramID]
	return m, ok
}

func (g *gate) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "inemgate"})
	})
	mux.HandleFunc("POST /v1/session", g.handleSession)
	mux.HandleFunc("GET /v1/verify", g.handleVerify)
	return mux
}

func (g *gate) handleSession(w http.ResponseWriter, r *http.Request) {
	var in struct {
		InitData string `json:"init_data"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if strings.TrimSpace(in.InitData) == "" {
		writeErr(w, http.StatusBadRequest, "init_data required")
		return
	}
	telegramID, name, err := verifyInitData(in.InitData, g.botToken, g.maxSkew, time.Now())
	switch {
	case errors.Is(err, errNoHash), errors.Is(err, errNoAuthDate), errors.Is(err, errNoUser):
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	m, ok := g.lookup(telegramID)
	if !ok {
		writeErr(w, http.StatusForbidden,
			fmt.Sprintf("telegram user %d is not a registered member of this household", telegramID))
		return
	}
	if name != "" && m.DisplayName == "" {
		m.DisplayName = name
	}
	s := session{UserID: m.UserID, TelegramUserID: m.TelegramUserID,
		ExpiresAt: time.Now().Add(g.ttl).Unix(), Name: m.DisplayName}
	token, err := mintSession(s, g.secret)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token": token, "user_id": s.UserID, "telegram_user_id": s.TelegramUserID,
		"display_name": s.Name, "expires_at": s.ExpiresAt,
	})
}

func (g *gate) handleVerify(w http.ResponseWriter, r *http.Request) {
	s, err := parseSession(r.URL.Query().Get("token"), g.secret, time.Now())
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id": s.UserID, "telegram_user_id": s.TelegramUserID,
		"display_name": s.Name, "expires_at": s.ExpiresAt,
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	g := &gate{
		botToken:  os.Getenv("TELEGRAM_BOT_TOKEN"),
		secret:    os.Getenv("INEM_GATE_SECRET"),
		inemBase:  env("INEM_BASE", "http://127.0.0.1:8777"),
		adminUser: env("INEM_GATE_ADMIN_USER", "1"),
		ttl:       time.Duration(atoiOr(env("INEM_GATE_TTL_HOURS", "720"), 720)) * time.Hour,
		maxSkew:   time.Duration(atoiOr(env("INEM_GATE_MAX_SKEW_SEC", "300"), 300)) * time.Second,
		client:    &http.Client{Timeout: 10 * time.Second},
	}
	if g.botToken == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN is required")
	}
	if len(g.secret) < 32 {
		log.Fatal("INEM_GATE_SECRET must be at least 32 characters")
	}
	addr := env("INEM_GATE_ADDR", "127.0.0.1:8778")
	srv := &http.Server{Addr: addr, Handler: g.handler(),
		ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second}
	log.Printf("inemgate listening on %s (inemd %s, admin user %s)", addr, g.inemBase, g.adminUser)
	log.Fatal(srv.ListenAndServe())
}

func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
