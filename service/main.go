// inemd — Mpok Inem domain services (expense, brain, nutrition) in one binary.
// Phase 0: DB-backed endpoints + uniform /internal/v1/handle contract (PRD TRD §4.2).
// Binds 127.0.0.1 only. No framework magic, explicit errors — Go/BRI house style.
package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	pq "github.com/lib/pq" // driver; pq.Array for text[] columns
)

var db *sql.DB

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func badReq(w http.ResponseWriter, msg string) { writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg}) }

func qID(s string) (int, error) { return strconv.Atoi(s) }

// ---------- uniform contract (TRD §4.2) ----------

type HandleReq struct {
	TraceID        string          `json:"trace_id"`
	ChatID         string          `json:"chat_id"`
	UserID         int             `json:"user_id"`
	Service        string          `json:"service"` // expense|brain|nutrition (set by caller)
	Text           string          `json:"text"`
	ImageRef       string          `json:"image_ref"`
	SessionContext json.RawMessage `json:"session_context"`
}

type HandleResp struct {
	ReplyText          string          `json:"reply_text"`
	RequiresConfirm    bool            `json:"requires_confirmation"`
	Confidence         float64         `json:"confidence"`
	SessionUpdate      json.RawMessage `json:"session_update,omitempty"`
}

// handlePOST: Phase 0 stub — validates identity, echoes structured ack.
func handlePOST(w http.ResponseWriter, r *http.Request) {
	var hr HandleReq
	if err := json.NewDecoder(r.Body).Decode(&hr); err != nil {
		badReq(w, "bad json: "+err.Error())
		return
	}
	if hr.UserID == 0 {
		badReq(w, "user_id required")
		return
	}
	var name string
	if err := db.QueryRow(`SELECT display_name FROM inem_auth.users WHERE id=$1`, hr.UserID).Scan(&name); err != nil {
		badReq(w, "unknown user_id")
		return
	}
	switch hr.Service {
	case "expense", "brain", "nutrition":
	default:
		badReq(w, "service must be expense|brain|nutrition")
		return
	}
	reply := fmt.Sprintf("[%s ack] user=%s trace=%s text=%q img=%s",
		hr.Service, name, hr.TraceID, hr.Text, hr.ImageRef)
	if hr.Text == "" && hr.ImageRef == "" {
		reply = fmt.Sprintf("[%s] user=%s: nothing to do (no text, no image)", hr.Service, name)
	}
	writeJSON(w, http.StatusOK, HandleResp{ReplyText: reply})
}

// ---------- expense ----------

type pocket struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Balance int64 `json:"balance_idr"`
}

// effective balance = opening + ins - outs + transfers in - out
const balanceSQL = `
SELECT p.id, p.name, p.type,
  p.opening_balance
  + COALESCE((SELECT SUM(CASE WHEN t.direction='in' THEN t.amount ELSE -t.amount END)
              FROM expense.transactions t WHERE t.pocket_id=p.id), 0)
  + COALESCE((SELECT SUM(tr.amount) FROM expense.transfers tr WHERE tr.to_pocket=p.id), 0)
  - COALESCE((SELECT SUM(tr.amount) FROM expense.transfers tr WHERE tr.from_pocket=p.id), 0)
  AS balance
FROM expense.pockets p WHERE p.user_id=$1 ORDER BY p.id`

func getPockets(w http.ResponseWriter, userID int) {
	rows, err := db.Query(balanceSQL, userID)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	defer rows.Close()
	out := []pocket{}
	for rows.Next() {
		var p pocket
		if err := rows.Scan(&p.ID, &p.Name, &p.Type, &p.Balance); err != nil {
			badReq(w, err.Error())
			return
		}
		out = append(out, p)
	}
	writeJSON(w, 200, out)
}

func createPocket(w http.ResponseWriter, r *http.Request, userID int) {
	var in struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Opening int64 `json:"opening_balance_idr"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badReq(w, err.Error())
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		badReq(w, "name required")
		return
	}
	if in.Type == "" {
		in.Type = "cash"
	}
	if in.Type != "cash" && in.Type != "savings" && in.Type != "investment" {
		badReq(w, "type must be cash|savings|investment")
		return
	}
	var id int
	err := db.QueryRow(`INSERT INTO expense.pockets(user_id,name,type,opening_balance) VALUES($1,$2,$3,$4) RETURNING id`,
		userID, in.Name, in.Type, in.Opening).Scan(&id)
	if err != nil && strings.Contains(err.Error(), "duplicate key") {
		badReq(w, "pocket name exists")
		return
	} else if err != nil {
		badReq(w, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id})
}

func createTxn(w http.ResponseWriter, r *http.Request, userID int) {
	var in struct {
		PocketID   int     `json:"pocket_id"`
		Direction  string  `json:"direction"`
		Amount     int64   `json:"amount_idr"`
		Category   string  `json:"category"`
		Note       string  `json:"note"`
		Source     string  `json:"source"`
		Confidence float64 `json:"confidence"`
		RawInput   string  `json:"raw_input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badReq(w, err.Error())
		return
	}
	if in.Direction != "in" && in.Direction != "out" {
		badReq(w, "direction must be in|out")
		return
	}
	if in.Amount <= 0 {
		badReq(w, "amount_idr must be > 0")
		return
	}
	if in.Source == "" {
		in.Source = "manual"
	}
	if in.Category == "" {
		in.Category = "general"
	}
	var ok int
	if err := db.QueryRow(`SELECT 1 FROM expense.pockets WHERE id=$1 AND user_id=$2`, in.PocketID, userID).Scan(&ok); err != nil {
		badReq(w, "pocket not found for user")
		return
	}
	var id int
	conf := any(nil)
	if in.Confidence > 0 {
		conf = in.Confidence
	}
	err := db.QueryRow(`INSERT INTO expense.transactions(user_id,pocket_id,direction,amount,category,note,source,confidence,raw_input)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
		userID, in.PocketID, in.Direction, in.Amount, in.Category, in.Note, in.Source, conf, in.RawInput).Scan(&id)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id})
}

func createTransfer(w http.ResponseWriter, r *http.Request, userID int) {
	var in struct {
		From int    `json:"from_pocket_id"`
		To   int    `json:"to_pocket_id"`
		Amount int64 `json:"amount_idr"`
		Note string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badReq(w, err.Error())
		return
	}
	if in.From == 0 || in.To == 0 || in.From == in.To || in.Amount <= 0 {
		badReq(w, "from/to/amount invalid")
		return
	}
	var bal int64
	err := db.QueryRow(`SELECT 1 FROM expense.pockets WHERE id=$1 AND user_id=$2`, in.From, userID).Scan(&bal)
	if err != nil {
		badReq(w, "source pocket not found for user")
		return
	}
	var id int
	err = db.QueryRow(`INSERT INTO expense.transfers(user_id,from_pocket,to_pocket,amount,note) VALUES($1,$2,$3,$4,$5) RETURNING id`,
		userID, in.From, in.To, in.Amount, in.Note).Scan(&id)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"id": id})
}

// summary: month rollup by category + pocket balances
func expenseSummary(w http.ResponseWriter, r *http.Request, userID int) {
	month := time.Now().Format("2006-01")
	if m := r.URL.Query().Get("month"); m != "" {
		if _, err := time.Parse("2006-01", m); err != nil {
			badReq(w, "month must be YYYY-MM")
			return
		}
		month = m
	}
	rows, err := db.Query(`SELECT category, direction, SUM(amount) FROM expense.transactions
		WHERE user_id=$1 AND to_char(created_at,'YYYY-MM')=$2 GROUP BY category, direction ORDER BY 3 DESC`, userID, month)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	defer rows.Close()
	type cat struct {
		Category string `json:"category"`
		Direction string `json:"direction"`
		TotalIDR int64 `json:"total_idr"`
	}
	cats := []cat{}
	for rows.Next() {
		var c cat
		if err := rows.Scan(&c.Category, &c.Direction, &c.TotalIDR); err != nil {
			badReq(w, err.Error())
			return
		}
		cats = append(cats, c)
	}
	pockets := []pocket{}
	prows, err := db.Query(balanceSQL, userID)
	if err == nil {
		defer prows.Close()
		for prows.Next() {
			var p pocket
			if prows.Scan(&p.ID, &p.Name, &p.Type, &p.Balance) == nil {
				pockets = append(pockets, p)
			}
		}
	}
	writeJSON(w, 200, map[string]any{"month": month, "by_category": cats, "pockets": pockets})
}

// ---------- brain ----------

type note struct {
	ID     int64    `json:"id"`
	Title  string   `json:"title"`
	BodyMD string   `json:"body_md"`
	Tags   []string `json:"tags"`
	Updated string  `json:"updated_at"`
}

func scanNote(s interface{ Scan(...any) error }) (note, error) {
	var n note
	var t time.Time
	err := s.Scan(&n.ID, &n.Title, &n.BodyMD, pq.Array(&n.Tags), &t)
	n.Updated = t.Format(time.RFC3339)
	return n, err
}

func createNote(w http.ResponseWriter, r *http.Request, userID int) {
	var in struct {
		Title  string   `json:"title"`
		BodyMD string   `json:"body_md"`
		Tags   []string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badReq(w, err.Error())
		return
	}
	if strings.TrimSpace(in.Title) == "" {
		badReq(w, "title required")
		return
	}
	var n note
	err := db.QueryRow(`INSERT INTO brain.notes(user_id,title,body_md,tags) VALUES($1,$2,$3,$4)
		RETURNING id,title,body_md,tags,updated_at`, userID, in.Title, in.BodyMD, pq.Array(in.Tags)).Scan(&n.ID, &n.Title, &n.BodyMD, pq.Array(&n.Tags), &n.Updated)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	writeJSON(w, 201, n)
}

func getNote(w http.ResponseWriter, r *http.Request, userID int, id int) {
	var n note
	err := db.QueryRow(`SELECT id,title,body_md,tags,updated_at FROM brain.notes WHERE id=$1 AND user_id=$2`, id, userID).
		Scan(&n.ID, &n.Title, &n.BodyMD, pq.Array(&n.Tags), &n.Updated)
	if errors.Is(err, sql.ErrNoRows) {
		badReq(w, "note not found")
		return
	} else if err != nil {
		badReq(w, err.Error())
		return
	}
	writeJSON(w, 200, n)
}

func searchNotes(w http.ResponseWriter, r *http.Request, userID int) {
	q := r.URL.Query().Get("q")
	if q == "" {
		badReq(w, "q required")
		return
	}
	rows, err := db.Query(`SELECT id,title,body_md,tags,updated_at FROM brain.notes
		WHERE user_id=$1 AND to_tsvector('simple', title || ' ' || body_md) @@ plainto_tsquery('simple', $2)
		ORDER BY ts_rank(to_tsvector('simple', title || ' ' || body_md), plainto_tsquery('simple', $2)) DESC LIMIT 20`,
		userID, q)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	defer rows.Close()
	out := []note{}
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			badReq(w, err.Error())
			return
		}
		out = append(out, n)
	}
	writeJSON(w, 200, out)
}

func listNotes(w http.ResponseWriter, r *http.Request, userID int) {
	rows, err := db.Query(`SELECT id,title,body_md,tags,updated_at FROM brain.notes WHERE user_id=$1 ORDER BY updated_at DESC LIMIT 50`, userID)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	defer rows.Close()
	out := []note{}
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			badReq(w, err.Error())
			return
		}
		out = append(out, n)
	}
	writeJSON(w, 200, out)
}

// ---------- nutrition ----------

func createMeal(w http.ResponseWriter, r *http.Request, userID int) {
	var in struct {
		PhotoRef string `json:"photo_ref"`
		Source   string `json:"source"`
		Note     string `json:"note"`
		Items    []struct {
			Name     string  `json:"name"`
			EstGrams int     `json:"est_grams"`
			Calories int     `json:"calories"`
			Protein  float64 `json:"protein"`
			Carbs    float64 `json:"carbs"`
			Fat      float64 `json:"fat"`
			Confidence float64 `json:"confidence"`
		} `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badReq(w, err.Error())
		return
	}
	if in.Source == "" {
		in.Source = "manual"
	}
	if in.Source != "manual" && in.Source != "photo" {
		badReq(w, "source must be manual|photo")
		return
	}
	tx, err := db.Begin()
	if err != nil {
		badReq(w, err.Error())
		return
	}
	defer tx.Rollback()
	var mealID int64
	err = tx.QueryRow(`INSERT INTO nutrition.meals(user_id,photo_ref,source,note) VALUES($1,$2,$3,$4) RETURNING id`,
		userID, in.PhotoRef, in.Source, in.Note).Scan(&mealID)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	for _, it := range in.Items {
		_, err = tx.Exec(`INSERT INTO nutrition.meal_items(meal_id,name,est_grams,calories,protein,carbs,fat,confidence)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
			mealID, it.Name, it.EstGrams, it.Calories, it.Protein, it.Carbs, it.Fat, it.Confidence)
		if err != nil {
			badReq(w, err.Error())
			return
		}
	}
	if err := tx.Commit(); err != nil {
		badReq(w, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"meal_id": mealID, "items": len(in.Items)})
}

func dailyRollup(w http.ResponseWriter, r *http.Request, userID int) {
	day := time.Now().Format("2006-01-02")
	if d := r.URL.Query().Get("day"); d != "" {
		if _, err := time.Parse("2006-01-02", d); err != nil {
			badReq(w, "day must be YYYY-MM-DD")
			return
		}
		day = d
	}
	type rollup struct {
		Day      string `json:"day"`
		Calories int    `json:"calories"`
		Protein  float64 `json:"protein_g"`
		Carbs    float64 `json:"carbs_g"`
		Fat      float64 `json:"fat_g"`
		Target   *int   `json:"calorie_target,omitempty"`
	}
	var r0 rollup
	var target sql.NullInt64
	err := db.QueryRow(`SELECT $1::date, COALESCE(SUM(i.calories),0), COALESCE(SUM(i.protein),0), COALESCE(SUM(i.carbs),0), COALESCE(SUM(i.fat),0),
		(SELECT calorie_target FROM nutrition.daily_targets WHERE user_id=$2 AND day=$1::date)
		FROM nutrition.meals m JOIN nutrition.meal_items i ON i.meal_id=m.id
		WHERE m.user_id=$2 AND m.logged_at=$1::date`, day, userID).
		Scan(&r0.Day, &r0.Calories, &r0.Protein, &r0.Carbs, &r0.Fat, &target)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	if target.Valid {
		t := int(target.Int64)
		r0.Target = &t
	}
	writeJSON(w, 200, r0)
}

// ---------- routing / auth ----------

// Phase 0 auth: X-User-ID header set by the local caller (Hermes agent on same box).
// Internal-only bind (127.0.0.1); swap for per-user tokens when a second machine appears.
func withUser(next func(w http.ResponseWriter, r *http.Request, uid int)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, err := qID(r.Header.Get("X-User-ID"))
		if err != nil || uid <= 0 {
			badReq(w, "X-User-ID header required")
			return
		}
		next(w, r, uid)
	}
}

func main() {
	dsn := os.Getenv("INEM_DSN")
	if dsn == "" {
		dsn = "postgres://inem@127.0.0.1:5432/inem?sslmode=disable"
	}
	var err error
	db, err = sql.Open("postgres", dsn)
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(8)
	for i := 0; i < 10; i++ {
		if err = db.Ping(); err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		log.Fatal("db unreachable: ", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok", "service": "inemd", "version": "0.1.0"})
	})
	mux.HandleFunc("POST /internal/v1/handle", handlePOST)

	mux.HandleFunc("GET /v1/pockets", withUser(func(w http.ResponseWriter, r *http.Request, uid int) { getPockets(w, uid) }))
	mux.HandleFunc("POST /v1/pockets", withUser(createPocket))
	mux.HandleFunc("POST /v1/transactions", withUser(createTxn))
	mux.HandleFunc("POST /v1/transfers", withUser(createTransfer))
	mux.HandleFunc("GET /v1/expense/summary", withUser(expenseSummary))

	mux.HandleFunc("POST /v1/notes", withUser(createNote))
	mux.HandleFunc("GET /v1/notes", withUser(listNotes))
	mux.HandleFunc("GET /v1/notes/search", withUser(searchNotes))
	mux.HandleFunc("GET /v1/notes/{id}", func(w http.ResponseWriter, r *http.Request) {
		uid, err := qID(r.Header.Get("X-User-ID"))
		id, err2 := qID(r.PathValue("id"))
		if err != nil || err2 != nil {
			badReq(w, "X-User-ID and note id required")
			return
		}
		getNote(w, r, uid, id)
	})

	mux.HandleFunc("POST /v1/meals", withUser(createMeal))
	mux.HandleFunc("GET /v1/nutrition/daily", withUser(dailyRollup))

	addr := os.Getenv("INEM_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8777"
	}
	srv := &http.Server{Addr: addr, Handler: mux, ReadTimeout: 15 * time.Second, WriteTimeout: 60 * time.Second}
	log.Printf("inemd listening on %s", addr)
	log.Fatal(srv.ListenAndServe())
}
