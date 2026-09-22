// inemdash — read-only dashboard for the inemd household data.
//
// Serves one static page plus a small JSON passthrough that talks to inemd on
// 127.0.0.1. The ledger is read-only through this binary: the only routes that
// write are the content ones (/api/ideas, /api/drafts and the editor's polish
// call), which deal in ideas and drafts about posts — never money. That is why
// there is no generic write passthrough, only those named routes.
//
// Identity comes from inemgate: Telegram signs the Mini App's initData, and
// only inemgate holds the bot token needed to check that signature. This binary
// therefore keeps no secret — it asks the gate to verify a session token and
// uses the returned inemd user id. The data endpoints then serve *that person's*
// data, so the wife sees her pockets and Dani sees his.
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed index.html
var indexHTML []byte

const sessionCookie = "inem_session"

type server struct {
	upstream string // inemd base URL
	gate     string // inemgate base URL
	userID   string // inemd user for break-glass password login
	authUser string // break-glass basic-auth user
	passwd   string // empty = no break-glass login at all
	client   *http.Client
	slow     *http.Client // for the LLM-backed call, which outlives the 15s budget
}

type identity struct {
	UserID int
	Name   string
	Via    string // "telegram" or "password"
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.serveIndex)
	mux.HandleFunc("GET /auth/logout", s.authLogout)
	mux.HandleFunc("POST /auth/telegram", s.authTelegram)
	mux.HandleFunc("POST /auth/password", s.authPassword)
	// whoami answers for everyone: the page uses it to decide between the app
	// and the login screen.
	mux.HandleFunc("GET /api/whoami", s.whoami)

	mux.HandleFunc("GET /api/pockets", s.authed(s.pass("/v1/pockets", nil)))
	mux.HandleFunc("GET /api/summary", s.authed(s.pass("/v1/expense/summary", periodQuery)))
	mux.HandleFunc("GET /api/txns", s.authed(s.pass("/v1/transactions", func(r *http.Request) (string, error) {
		q, err := periodQuery(r)
		if err != nil {
			return "", err
		}
		return q + "&limit=" + limitOr(r, "100", "500"), nil
	})))
	mux.HandleFunc("GET /api/pocket", s.authed(s.pocketDetail))
	mux.HandleFunc("GET /api/transfers", s.authed(s.pass("/v1/transfers", func(r *http.Request) (string, error) {
		q, err := periodQuery(r)
		if err != nil {
			return "", err
		}
		return q + "&limit=" + limitOr(r, "100", "500"), nil
	})))
	mux.HandleFunc("GET /api/household", s.authed(s.pass("/v1/household", nil)))
	mux.HandleFunc("GET /api/schedules", s.authed(s.pass("/v1/schedules", nil)))
	mux.HandleFunc("GET /api/notes", s.authed(s.proxyNotes))
	mux.HandleFunc("GET /api/notes/{id}", s.authed(s.pass("/v1/notes/{id}", nil)))
	mux.HandleFunc("GET /api/meals", s.authed(s.pass("/v1/meals", func(r *http.Request) (string, error) {
		return "?day=" + dayOr(r, time.Now().Format("2006-01-02")), nil
	})))
	mux.HandleFunc("GET /api/day", s.authed(s.pass("/v1/nutrition/daily", func(r *http.Request) (string, error) {
		return "?day=" + dayOr(r, time.Now().Format("2006-01-02")), nil
	})))
	mux.HandleFunc("GET /api/target", s.authed(s.pass("/v1/nutrition/target", func(r *http.Request) (string, error) {
		return "?day=" + dayOr(r, time.Now().Format("2006-01-02")), nil
	})))

	// Bank ide + editor. The dashboard was read-only by design and stays so for
	// the ledger: every write below touches content.* only — ideas and drafts,
	// never money. That is why they are spelled out one by one instead of a
	// generic passthrough.
	mux.HandleFunc("GET /api/ideas", s.authed(s.pass("/v1/content/topics", func(r *http.Request) (string, error) {
		return "?status=" + statusOr(r, "new") + "&limit=" + limitOr(r, "100", "200"), nil
	})))
	mux.HandleFunc("PATCH /api/ideas/{id}", s.authed(s.ideaWrite))
	mux.HandleFunc("POST /api/ideas/polish", s.authed(s.polish))
	mux.HandleFunc("GET /api/drafts", s.authed(s.pass("/v1/content/drafts", func(r *http.Request) (string, error) {
		return "?status=" + statusOr(r, "pending") + "&limit=" + limitOr(r, "50", "200"), nil
	})))
	mux.HandleFunc("POST /api/drafts", s.authed(s.draftWrite))
	mux.HandleFunc("PATCH /api/drafts/{id}", s.authed(s.draftUpdate))
	return mux
}

func statusOr(r *http.Request, fallback string) string {
	v := strings.TrimSpace(r.URL.Query().Get("status"))
	if v == "" {
		return fallback
	}
	return urlQueryEscape(v)
}

// ideaWrite lets an idea be marked used or skipped from the page.
func (s *server) ideaWrite(w http.ResponseWriter, r *http.Request, id identity) {
	pid, err := pathID(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad id"})
		return
	}
	s.send(w, r, http.MethodPatch, "/v1/content/topics/"+pid, id.UserID)
}

// polish fronts the engine's "rapikan dengan AI"; inemd proxies it on.
func (s *server) polish(w http.ResponseWriter, r *http.Request, id identity) {
	s.sendSlow(w, r, http.MethodPost, "/v1/content/polish", id.UserID)
}

// draftWrite is the editor's save: a post Dani typed himself goes into the queue.
func (s *server) draftWrite(w http.ResponseWriter, r *http.Request, id identity) {
	s.send(w, r, http.MethodPost, "/v1/content/drafts", id.UserID)
}

// draftUpdate is approve/reject/edit — the review step, from the page.
func (s *server) draftUpdate(w http.ResponseWriter, r *http.Request, id identity) {
	pid, err := pathID(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad id"})
		return
	}
	s.send(w, r, http.MethodPatch, "/v1/content/drafts/"+pid, id.UserID)
}

func pathID(r *http.Request) (string, error) {
	pid := strings.TrimSpace(r.PathValue("id"))
	if _, err := strconv.Atoi(pid); pid == "" || err != nil {
		return "", fmt.Errorf("id must be a positive integer")
	}
	return pid, nil
}

// ---------- identity ----------

// resolve reads the session cookie (verified by the gate) or the break-glass
// password. Everything else is anonymous.
func (s *server) resolve(r *http.Request) (identity, bool) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if id, err := s.verifySession(r, c.Value); err == nil {
			return id, true
		}
	}
	if s.passwd != "" {
		user, pass, ok := r.BasicAuth()
		if ok && user == s.authUser && pass == s.passwd {
			uid, err := strconv.Atoi(s.userID)
			if err != nil || uid <= 0 {
				return identity{}, false
			}
			return identity{UserID: uid, Name: s.authUser, Via: "password"}, true
		}
	}
	return identity{}, false
}

// verifySession asks inemgate whether a session token is genuine and fresh.
func (s *server) verifySession(r *http.Request, token string) (identity, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet,
		s.gate+"/v1/verify?token="+urlQueryEscape(token), nil)
	if err != nil {
		return identity{}, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return identity{}, fmt.Errorf("gate unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return identity{}, fmt.Errorf("gate rejected the session: %s", strings.TrimSpace(string(raw)))
	}
	var out struct {
		UserID      int    `json:"user_id"`
		DisplayName string `json:"display_name"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.UserID == 0 {
		return identity{}, fmt.Errorf("gate returned an unusable session")
	}
	return identity{UserID: out.UserID, Name: out.DisplayName, Via: "telegram"}, nil
}

// authed guards every data endpoint: no identity, no data.
func (s *server) authed(next func(w http.ResponseWriter, r *http.Request, id identity)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.resolve(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not authenticated"})
			return
		}
		next(w, r, id)
	}
}

func (s *server) whoami(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolve(r)
	out := map[string]any{
		"authenticated":  ok,
		"user_id":        id.UserID,
		"display_name":   id.Name,
		"via":            id.Via,
		"password_login": s.passwd != "",
		"scope":          "",
	}
	// Which tabs to show is a property of the member, not of the browser: ask
	// inemd who this is rather than trusting anything the page sent.
	if ok {
		if body, code, err := s.get(r, "/v1/me", id.UserID); err == nil && code == http.StatusOK {
			var me struct {
				DisplayName string `json:"display_name"`
				Scope       string `json:"scope"`
			}
			if json.Unmarshal(body, &me) == nil {
				if me.DisplayName != "" {
					out["display_name"] = me.DisplayName
				}
				out["scope"] = me.Scope
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// authTelegram takes the Mini App initData, has the gate check Telegram's
// signature, and turns a good one into a session cookie.
func (s *server) authTelegram(w http.ResponseWriter, r *http.Request) {
	var in struct {
		InitData string `json:"init_data"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	if strings.TrimSpace(in.InitData) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "init_data required"})
		return
	}
	body, _ := json.Marshal(map[string]string{"init_data": in.InitData})
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.gate+"/v1/session", bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "gate unreachable"})
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw) // the gate's reason ("not a registered member", ...)
		return
	}
	var out struct {
		Token       string `json:"token"`
		UserID      int    `json:"user_id"`
		DisplayName string `json:"display_name"`
		ExpiresAt   int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "gate returned no session"})
		return
	}
	s.setSessionCookie(w, r, out.Token, out.ExpiresAt)
	writeJSON(w, http.StatusOK, map[string]any{"user_id": out.UserID, "display_name": out.DisplayName})
}

// authPassword is the break-glass path: only active when a password is set.
func (s *server) authPassword(w http.ResponseWriter, r *http.Request) {
	if s.passwd == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "password login is disabled"})
		return
	}
	user, pass, ok := r.BasicAuth()
	if !ok {
		var in struct {
			User     string `json:"user"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err == nil {
			user, pass, ok = in.User, in.Password, true
		}
	}
	if !ok || user != s.authUser || pass != s.passwd {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "wrong user or password"})
		return
	}
	s.setSessionCookie(w, r, "", 0) // no gate token: basic auth is re-checked per request
	writeJSON(w, http.StatusOK, map[string]any{"user_id": s.userID, "display_name": s.authUser})
}

func (s *server) authLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: cookiePath(r),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expiresAt int64) {
	c := &http.Cookie{
		Name: sessionCookie, Value: token, Path: cookiePath(r),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	}
	if expiresAt > 0 {
		c.MaxAge = int(time.Until(time.Unix(expiresAt, 0)).Seconds())
	}
	http.SetCookie(w, c)
}

// cookiePath keeps the cookie scoped to where the app is mounted (/inem/ when
// nginx proxies it, / when the dashboard is served at the root).
func cookiePath(r *http.Request) string {
	p := r.URL.Path
	if strings.HasPrefix(p, "/inem/") || p == "/inem" {
		return "/inem/"
	}
	return "/"
}

// ---------- pages and passthrough ----------

func (s *server) serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/inem" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

type queryFunc func(*http.Request) (string, error)

// pass forwards a GET to inemd as the authenticated user. path may contain
// {id}, taken from the incoming path.
func (s *server) pass(path string, q queryFunc) func(http.ResponseWriter, *http.Request, identity) {
	return func(w http.ResponseWriter, r *http.Request, id identity) {
		target := path
		if pid := r.PathValue("id"); pid != "" {
			if _, err := strconv.Atoi(pid); err != nil {
				http.Error(w, "bad id", http.StatusBadRequest)
				return
			}
			target = strings.ReplaceAll(target, "{id}", pid)
		}
		if q != nil {
			query, err := q(r)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			target += query
		}
		s.forward(w, r, target, id.UserID)
	}
}

// proxyNotes: list, tag-filter, or full-text search — one endpoint for the page.
func (s *server) proxyNotes(w http.ResponseWriter, r *http.Request, id identity) {
	q := r.URL.Query()
	if term := strings.TrimSpace(q.Get("q")); term != "" {
		s.forward(w, r, "/v1/notes/search?q="+urlQueryEscape(term), id.UserID)
		return
	}
	if tag := strings.TrimSpace(q.Get("tag")); tag != "" {
		s.forward(w, r, "/v1/notes?limit=100&tag="+urlQueryEscape(tag), id.UserID)
		return
	}
	s.forward(w, r, "/v1/notes?limit=100", id.UserID)
}

func (s *server) forward(w http.ResponseWriter, r *http.Request, target string, uid int) {
	body, code, err := s.get(r, target, uid)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// get performs the upstream GET and returns the body plus inemd's status code
// (an error response is still JSON and still belongs to the caller).
func (s *server) get(r *http.Request, target string, uid int) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, s.upstream+target, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-User-ID", strconv.Itoa(uid))
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("inemd unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, 0, err
	}
	return body, resp.StatusCode, nil
}

// send is the write path, used only by the content routes. The ledger stays
// read-only through this binary: nothing here can move money.
func (s *server) send(w http.ResponseWriter, r *http.Request, method, target string, uid int) {
	s.sendWith(s.client, w, r, method, target, uid)
}

// sendSlow is for the one call that waits on an LLM. The default 15s budget is
// right for reading a ledger and far too short for "rapikan dengan AI", which
// showed up as "inemd unreachable" while the engine was working fine.
func (s *server) sendSlow(w http.ResponseWriter, r *http.Request, method, target string, uid int) {
	s.sendWith(s.slow, w, r, method, target, uid)
}

func (s *server) sendWith(c *http.Client, w http.ResponseWriter, r *http.Request, method, target string, uid int) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 256<<10))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable body"})
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), method, s.upstream+target, bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	req.Header.Set("X-User-ID", strconv.Itoa(uid))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "inemd unreachable: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(raw)
}

// fetch is a GET that treats a non-2xx as an error (used by the composed view).
func (s *server) fetch(r *http.Request, target string, uid int) ([]byte, error) {
	body, code, err := s.get(r, target, uid)
	if err != nil {
		return nil, err
	}
	if code < 200 || code > 299 {
		return nil, fmt.Errorf("inemd %s: %s", target, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// pocketDetail serves one pocket's ledger: the pocket itself, its own entries,
// and the transfers in or out of it — merged into one response so the page does
// not have to stitch three calls together.
func (s *server) pocketDetail(w http.ResponseWriter, r *http.Request, id identity) {
	pid := strings.TrimSpace(r.URL.Query().Get("id"))
	if _, err := strconv.Atoi(pid); pid == "" || err != nil {
		http.Error(w, "id must be a positive integer", http.StatusBadRequest)
		return
	}
	period, err := periodQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	pocket, err := s.fetch(r, "/v1/pockets/"+pid, id.UserID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	txns, err := s.fetch(r, "/v1/transactions"+period+"&pocket_id="+pid+"&limit=500", id.UserID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	transfers, err := s.fetch(r, "/v1/transfers"+period+"&pocket_id="+pid+"&limit=500", id.UserID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	type movement struct {
		Kind         string `json:"kind"`
		ID           int64  `json:"id"`
		At           string `json:"at"`
		Direction    string `json:"direction"`
		AmountIDR    int64  `json:"amount_idr"`
		Label        string `json:"label"`
		Category     string `json:"category,omitempty"`
		Counterparty string `json:"counterparty,omitempty"`
	}
	movements := []movement{}
	var out, in, moveOut, moveIn int64

	var txnRows []struct {
		ID        int64  `json:"id"`
		Direction string `json:"direction"`
		AmountIDR int64  `json:"amount_idr"`
		Category  string `json:"category"`
		Note      string `json:"note"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(txns, &txnRows); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "bad transactions payload"})
		return
	}
	for _, t := range txnRows {
		label := t.Note
		if label == "" {
			label = t.Category
		}
		movements = append(movements, movement{
			Kind: "txn", ID: t.ID, At: t.CreatedAt, Direction: t.Direction,
			AmountIDR: t.AmountIDR, Label: label, Category: t.Category,
		})
		if t.Direction == "out" {
			out += t.AmountIDR
		} else {
			in += t.AmountIDR
		}
	}

	var transferRows []struct {
		ID         int64  `json:"id"`
		FromPocket int    `json:"from_pocket_id"`
		From       string `json:"from"`
		To         string `json:"to"`
		AmountIDR  int64  `json:"amount_idr"`
		Note       string `json:"note"`
		CreatedAt  string `json:"created_at"`
	}
	if err := json.Unmarshal(transfers, &transferRows); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "bad transfers payload"})
		return
	}
	for _, t := range transferRows {
		dir, counter, label := "in", t.From, "← "+t.From
		moveIn += t.AmountIDR
		if fmt.Sprint(t.FromPocket) == pid {
			dir, counter, label = "out", t.To, "→ "+t.To
			moveIn -= t.AmountIDR
			moveOut += t.AmountIDR
		}
		if t.Note != "" {
			label += " · " + t.Note
		}
		movements = append(movements, movement{
			Kind: "transfer", ID: t.ID, At: t.CreatedAt, Direction: dir,
			AmountIDR: t.AmountIDR, Label: label, Counterparty: counter,
		})
	}

	sort.SliceStable(movements, func(i, j int) bool { return movements[i].At > movements[j].At })

	writeJSON(w, http.StatusOK, map[string]any{
		"pocket":    json.RawMessage(pocket),
		"totals":    map[string]any{"out_idr": out, "in_idr": in, "transfer_out_idr": moveOut, "transfer_in_idr": moveIn},
		"movements": movements,
		"period":    period,
		"user_id":   id.UserID,
	})
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func urlQueryEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func dayOr(r *http.Request, def string) string {
	if d := r.URL.Query().Get("day"); d != "" {
		if _, err := time.Parse("2006-01-02", d); err == nil {
			return d
		}
	}
	return def
}

func limitOr(r *http.Request, def, max string) string {
	l := r.URL.Query().Get("limit")
	if l == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(l, "%d", &n); err != nil || n <= 0 || fmt.Sprint(n) != l {
		return def
	}
	var m int
	_, _ = fmt.Sscanf(max, "%d", &m)
	if n > m {
		return max
	}
	return fmt.Sprint(n)
}

// periodQuery mirrors the skill's period vocabulary: today|yesterday|week|
// last-week|month|last-month, an explicit YYYY-MM-DD..YYYY-MM-DD range, or a
// plain YYYY-MM month. Weeks run Monday→Sunday.
func periodQuery(r *http.Request) (string, error) {
	p := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("period")))
	today := time.Now()
	switch {
	case p == "" || p == "month":
		return "?month=" + today.Format("2006-01"), nil
	case p == "today":
		d := today.Format("2006-01-02")
		return "?from=" + d + "&to=" + d, nil
	case p == "yesterday":
		d := today.AddDate(0, 0, -1).Format("2006-01-02")
		return "?from=" + d + "&to=" + d, nil
	case p == "week":
		start := monday(today)
		return "?from=" + start.Format("2006-01-02") + "&to=" + today.Format("2006-01-02"), nil
	case p == "last-week":
		start := monday(today).AddDate(0, 0, -7)
		return "?from=" + start.Format("2006-01-02") + "&to=" + start.AddDate(0, 0, 6).Format("2006-01-02"), nil
	case p == "last-month":
		first := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, today.Location())
		last := first.AddDate(0, 0, -1)
		return "?month=" + last.Format("2006-01"), nil
	case strings.Contains(p, ".."):
		from, to, ok := strings.Cut(p, "..")
		if !ok || !validDate(from) || !validDate(to) {
			return "", fmt.Errorf("range must be YYYY-MM-DD..YYYY-MM-DD")
		}
		return "?from=" + from + "&to=" + to, nil
	case len(p) == 7 && validMonth(p):
		return "?month=" + p, nil
	default:
		return "", fmt.Errorf("unknown period %q", p)
	}
}

func validDate(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

func validMonth(s string) bool {
	_, err := time.Parse("2006-01", s)
	return err == nil
}

func monday(t time.Time) time.Time {
	offset := (int(t.Weekday()) + 6) % 7 // Monday=0
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).AddDate(0, 0, -offset)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	s := &server{
		upstream: env("INEM_BASE", "http://127.0.0.1:8777"),
		gate:     env("INEM_GATE_BASE", "http://127.0.0.1:8778"),
		userID:   env("INEM_WEB_USER", "1"),
		authUser: env("INEM_WEB_AUTH_USER", "dani"),
		passwd:   os.Getenv("INEM_WEB_AUTH_PASS"),
		client:   &http.Client{Timeout: 15 * time.Second},
		slow:     &http.Client{Timeout: 120 * time.Second},
	}
	addr := env("INEM_WEB_ADDR", "127.0.0.1:8090")
	if s.passwd == "" {
		log.Printf("break-glass password login disabled (INEM_WEB_AUTH_PASS unset)")
	}
	log.Printf("inemdash listening on %s (inemd %s, gate %s)", addr, s.upstream, s.gate)
	// WriteTimeout covers the polish call too: the browser waits for the model
	srv := &http.Server{Addr: addr, Handler: s.handler(), ReadTimeout: 15 * time.Second, WriteTimeout: 180 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
