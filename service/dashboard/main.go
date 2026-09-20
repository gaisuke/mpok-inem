// inemdash — read-only dashboard for the inemd household data.
//
// Serves one static page plus a small JSON passthrough that talks to inemd on
// 127.0.0.1. Only GET is routed: there is no write path through this binary at
// all, so exposing it (nginx + TLS + basic auth) can never change the ledger.
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

//go:embed index.html
var indexHTML []byte

type server struct {
	upstream string // inemd base URL, e.g. http://127.0.0.1:8777
	userID   string // X-User-ID inemd expects
	authUser string // optional basic-auth user (empty passwd = no auth)
	passwd   string
	client   *http.Client
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.serveIndex)
	mux.HandleFunc("GET /api/pockets", s.pass("/v1/pockets", nil))
	mux.HandleFunc("GET /api/summary", s.pass("/v1/expense/summary", periodQuery))
	mux.HandleFunc("GET /api/txns", s.pass("/v1/transactions", func(r *http.Request) (string, error) {
		q, err := periodQuery(r)
		if err != nil {
			return "", err
		}
		return q + "&limit=" + limitOr(r, "100", "500"), nil
	}))
	mux.HandleFunc("GET /api/pocket", s.proxyPocketDetail)
	mux.HandleFunc("GET /api/notes", s.proxyNotes)
	mux.HandleFunc("GET /api/notes/{id}", s.pass("/v1/notes/{id}", nil))
	mux.HandleFunc("GET /api/meals", s.pass("/v1/meals", func(r *http.Request) (string, error) {
		return "?day=" + dayOr(r, time.Now().Format("2006-01-02")), nil
	}))
	mux.HandleFunc("GET /api/day", s.pass("/v1/nutrition/daily", func(r *http.Request) (string, error) {
		return "?day=" + dayOr(r, time.Now().Format("2006-01-02")), nil
	}))
	mux.HandleFunc("GET /api/target", s.pass("/v1/nutrition/target", func(r *http.Request) (string, error) {
		return "?day=" + dayOr(r, time.Now().Format("2006-01-02")), nil
	}))
	return s.withAuth(mux)
}

func (s *server) serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

// withAuth adds HTTP basic auth when a password is configured. The dashboard
// shows household money, so a deployment that is reachable off-box must set one.
func (s *server) withAuth(next http.Handler) http.Handler {
	if s.passwd == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != s.authUser || pass != s.passwd {
			w.Header().Set("WWW-Authenticate", `Basic realm="mpok inem"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// pass forwards a GET to inemd, rewriting the query with q (when given).
// path may contain {id}, taken from the incoming path.
func (s *server) pass(path string, q func(*http.Request) (string, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		target := path
		if id := r.PathValue("id"); id != "" {
			if _, err := fmt.Sscanf(id, "%d", new(int)); err != nil {
				badGateway(w, "bad id")
				return
			}
			target = strings.ReplaceAll(target, "{id}", id)
		}
		if q != nil {
			query, err := q(r)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			target += query
		}
		s.forward(w, r, target)
	}
}

// proxyPocketDetail serves one pocket's ledger: the pocket itself, its own
// entries, and the transfers in or out of it — merged into one response so the
// page does not have to stitch three calls together.
func (s *server) proxyPocketDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if _, err := fmt.Sscanf(id, "%d", new(int)); id == "" || err != nil {
		http.Error(w, "id must be a positive integer", http.StatusBadRequest)
		return
	}
	period, err := periodQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	pocket, err := s.fetch("/v1/pockets/" + id)
	if err != nil {
		badGateway(w, err.Error())
		return
	}
	txns, err := s.fetch("/v1/transactions" + period + "&pocket_id=" + id + "&limit=500")
	if err != nil {
		badGateway(w, err.Error())
		return
	}
	transfers, err := s.fetch("/v1/transfers" + period + "&pocket_id=" + id + "&limit=500")
	if err != nil {
		badGateway(w, err.Error())
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
		badGateway(w, "bad transactions payload: "+err.Error())
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
		badGateway(w, "bad transfers payload: "+err.Error())
		return
	}
	for _, t := range transferRows {
		dir, counter, label := "in", t.From, "← "+t.From
		moveIn += t.AmountIDR
		if fmt.Sprint(t.FromPocket) == id {
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

	resp := map[string]any{
		"pocket":    json.RawMessage(pocket),
		"totals":    map[string]any{"out_idr": out, "in_idr": in, "transfer_out_idr": moveOut, "transfer_in_idr": moveIn},
		"movements": movements,
		"period":    period,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// fetch is a GET against inemd that returns the body or an error for a non-2xx.
func (s *server) fetch(target string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, s.upstream+target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-User-ID", s.userID)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("inemd %s: %s", target, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// proxyNotes: list, tag-filter, or full-text search — one endpoint for the page.
func (s *server) proxyNotes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if term := strings.TrimSpace(q.Get("q")); term != "" {
		s.forward(w, r, "/v1/notes/search?q="+urlQueryEscape(term))
		return
	}
	if tag := strings.TrimSpace(q.Get("tag")); tag != "" {
		s.forward(w, r, "/v1/notes?limit=100&tag="+urlQueryEscape(tag))
		return
	}
	s.forward(w, r, "/v1/notes?limit=100")
}

func (s *server) forward(w http.ResponseWriter, r *http.Request, target string) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, s.upstream+target, nil)
	if err != nil {
		badGateway(w, err.Error())
		return
	}
	req.Header.Set("X-User-ID", s.userID)
	resp, err := s.client.Do(req)
	if err != nil {
		badGateway(w, "inemd unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		badGateway(w, err.Error())
		return
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	// passthrough of a JSON error is still JSON: keep the upstream status
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func badGateway(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
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
		userID:   env("INEM_WEB_USER", "1"),
		authUser: env("INEM_WEB_AUTH_USER", "inem"),
		passwd:   os.Getenv("INEM_WEB_AUTH_PASS"),
		client:   &http.Client{Timeout: 15 * time.Second},
	}
	addr := env("INEM_WEB_ADDR", "127.0.0.1:8090")
	if s.passwd == "" {
		log.Printf("warning: INEM_WEB_AUTH_PASS is unset — no authentication")
	}
	log.Printf("inemdash listening on %s (inemd %s, user %s)", addr, s.upstream, s.userID)
	srv := &http.Server{Addr: addr, Handler: s.handler(), ReadTimeout: 15 * time.Second, WriteTimeout: 60 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
