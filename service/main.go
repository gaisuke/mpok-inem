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

func badReq(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
}

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
	ReplyText       string          `json:"reply_text"`
	RequiresConfirm bool            `json:"requires_confirmation"`
	Confidence      float64         `json:"confidence"`
	SessionUpdate   json.RawMessage `json:"session_update,omitempty"`
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
	ID         int    `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Visibility string `json:"visibility"` // private | shared
	Balance    int64  `json:"balance_idr"`
}

// effective balance = opening + ins - outs + transfers in - out
const balanceExpr = `
  p.opening_balance
  + COALESCE((SELECT SUM(CASE WHEN t.direction='in' THEN t.amount ELSE -t.amount END)
              FROM expense.transactions t WHERE t.pocket_id=p.id), 0)
  + COALESCE((SELECT SUM(tr.amount) FROM expense.transfers tr WHERE tr.to_pocket=p.id), 0)
  - COALESCE((SELECT SUM(tr.amount) FROM expense.transfers tr WHERE tr.from_pocket=p.id), 0)`

const balanceSQL = `SELECT p.id, p.name, p.type, p.visibility,` + balanceExpr + ` AS balance
FROM expense.pockets p WHERE p.user_id=$1 ORDER BY p.id`

// pocketSQL reads one pocket the caller may see: their own, or one its owner
// shared. Sharing is read-only — writes stay scoped by user_id everywhere else.
const pocketSQL = `SELECT p.id, p.name, p.type, p.visibility,` + balanceExpr + ` AS balance
FROM expense.pockets p WHERE p.id=$1 AND (p.user_id=$2 OR p.visibility='shared')`

func scanPocket(s interface{ Scan(...any) error }) (pocket, error) {
	var p pocket
	err := s.Scan(&p.ID, &p.Name, &p.Type, &p.Visibility, &p.Balance)
	return p, err
}

// pocketVisibleTo is the read check used by the entry lists: the caller owns the
// pocket, or the owner shared it.
func pocketVisibleTo(userID, pocketID int) (bool, error) {
	var ok int
	err := db.QueryRow(`SELECT 1 FROM expense.pockets WHERE id=$1 AND (user_id=$2 OR visibility='shared')`,
		pocketID, userID).Scan(&ok)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func getPockets(w http.ResponseWriter, userID int) {
	rows, err := db.Query(balanceSQL, userID)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	defer rows.Close()
	out := []pocket{}
	for rows.Next() {
		p, err := scanPocket(rows)
		if err != nil {
			badReq(w, err.Error())
			return
		}
		out = append(out, p)
	}
	writeJSON(w, 200, out)
}

func createPocket(w http.ResponseWriter, r *http.Request, userID int) {
	var in struct {
		Name       string `json:"name"`
		Type       string `json:"type"`
		Opening    int64  `json:"opening_balance_idr"`
		Visibility string `json:"visibility"`
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
	// private unless the owner says otherwise: sharing is opt-in, never a default
	if in.Visibility == "" {
		in.Visibility = "private"
	}
	if in.Visibility != "private" && in.Visibility != "shared" {
		badReq(w, "visibility must be private|shared")
		return
	}
	var id int
	err := db.QueryRow(`INSERT INTO expense.pockets(user_id,name,type,opening_balance,visibility) VALUES($1,$2,$3,$4,$5) RETURNING id`,
		userID, in.Name, in.Type, in.Opening, in.Visibility).Scan(&id)
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

// createTransfer moves money between pockets. The source must be the caller's
// own pocket — nobody spends someone else's money — while the destination may be
// another member's pocket, as long as the caller can see it (shared): that is the
// "I gave my wife some money" case. A private pocket the caller cannot see is
// reported exactly like a pocket that does not exist, so names cannot be probed.
func createTransfer(w http.ResponseWriter, r *http.Request, userID int) {
	var in struct {
		From   int    `json:"from_pocket_id"`
		To     int    `json:"to_pocket_id"`
		Amount int64  `json:"amount_idr"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badReq(w, err.Error())
		return
	}
	if in.From == 0 || in.To == 0 || in.From == in.To || in.Amount <= 0 {
		badReq(w, "from/to/amount invalid")
		return
	}
	var owned int
	err := db.QueryRow(`SELECT 1 FROM expense.pockets WHERE id=$1 AND user_id=$2`, in.From, userID).Scan(&owned)
	if err != nil {
		badReq(w, "source pocket not found for user")
		return
	}
	visible, err := pocketVisibleTo(userID, in.To)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	if !visible {
		badReq(w, "destination pocket not found")
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

// summary: category rollup + totals for a period (month, or from/to) + pocket balances
func expenseSummary(w http.ResponseWriter, r *http.Request, userID int) {
	q := r.URL.Query()
	month, from, to := q.Get("month"), q.Get("from"), q.Get("to")
	switch {
	case from != "" || to != "":
		today := time.Now().Format("2006-01-02")
		if from == "" {
			from = today
		}
		if to == "" {
			to = today
		}
		for _, d := range []struct{ name, val string }{{"from", from}, {"to", to}} {
			if _, err := time.Parse("2006-01-02", d.val); err != nil {
				badReq(w, d.name+" must be YYYY-MM-DD")
				return
			}
		}
		if to < from {
			badReq(w, "to must not be before from")
			return
		}
		month = ""
	default:
		if month == "" {
			month = time.Now().Format("2006-01")
		}
		if _, err := time.Parse("2006-01", month); err != nil {
			badReq(w, "month must be YYYY-MM")
			return
		}
	}

	where, args := "to_char(created_at,'YYYY-MM')=$2", []any{userID, month}
	if month == "" {
		where, args = "created_at::date BETWEEN $2::date AND $3::date", []any{userID, from, to}
	}

	rows, err := db.Query(`SELECT category, direction, SUM(amount) FROM expense.transactions
		WHERE user_id=$1 AND `+where+` GROUP BY category, direction ORDER BY 3 DESC`, args...)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	defer rows.Close()
	type cat struct {
		Category  string `json:"category"`
		Direction string `json:"direction"`
		TotalIDR  int64  `json:"total_idr"`
	}
	cats := []cat{}
	var totalOut, totalIn int64
	for rows.Next() {
		var c cat
		if err := rows.Scan(&c.Category, &c.Direction, &c.TotalIDR); err != nil {
			badReq(w, err.Error())
			return
		}
		cats = append(cats, c)
		if c.Direction == "out" {
			totalOut += c.TotalIDR
		} else {
			totalIn += c.TotalIDR
		}
	}
	pockets := []pocket{}
	prows, err := db.Query(balanceSQL, userID)
	if err == nil {
		defer prows.Close()
		for prows.Next() {
			if p, err := scanPocket(prows); err == nil {
				pockets = append(pockets, p)
			}
		}
	}
	resp := map[string]any{
		"by_category": cats, "pockets": pockets,
		"total_out_idr": totalOut, "total_in_idr": totalIn,
	}
	if month != "" {
		resp["month"] = month
	} else {
		resp["from"], resp["to"] = from, to
	}
	writeJSON(w, 200, resp)
}

// listTxns: transactions for a period, newest first (detail behind a summary).
// With ?pocket_id= it serves that pocket's ledger — the caller's own, or one
// shared with them (access checked first, so a shared pocket's entries are
// readable while a private one stays invisible).
func listTxns(w http.ResponseWriter, r *http.Request, userID int) {
	q := r.URL.Query()
	limit := 50
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n <= 0 || n > 500 {
			badReq(w, "limit must be 1..500")
			return
		}
		limit = n
	}
	args := []any{}
	where := ""
	if p := q.Get("pocket_id"); p != "" {
		pid, err := strconv.Atoi(p)
		if err != nil || pid <= 0 {
			badReq(w, "pocket_id must be a positive integer")
			return
		}
		ok, err := pocketVisibleTo(userID, pid)
		if err != nil {
			badReq(w, err.Error())
			return
		}
		if !ok {
			forbidden(w, "pocket is not yours and has not been shared")
			return
		}
		// access is proven, so the pocket alone scopes the query — a shared
		// pocket's entries belong to its owner, not to the caller
		args = append(args, pid)
		where = "t.pocket_id=$1"
	} else {
		args = append(args, userID)
		where = "t.user_id=$1"
	}
	from, to := q.Get("from"), q.Get("to")
	if from != "" || to != "" {
		if from == "" {
			from = to
		}
		if to == "" {
			to = from
		}
		for _, d := range []struct{ name, val string }{{"from", from}, {"to", to}} {
			if _, err := time.Parse("2006-01-02", d.val); err != nil {
				badReq(w, d.name+" must be YYYY-MM-DD")
				return
			}
		}
		if to < from {
			badReq(w, "to must not be before from")
			return
		}
		args = append(args, from, to)
		where += fmt.Sprintf(" AND t.created_at::date BETWEEN $%d::date AND $%d::date", len(args)-1, len(args))
	}
	if c := q.Get("category"); c != "" {
		args = append(args, c)
		where += fmt.Sprintf(" AND t.category=$%d", len(args))
	}
	if d := q.Get("direction"); d != "" {
		if d != "in" && d != "out" {
			badReq(w, "direction must be in|out")
			return
		}
		args = append(args, d)
		where += fmt.Sprintf(" AND t.direction=$%d", len(args))
	}
	args = append(args, limit)
	rows, err := db.Query(fmt.Sprintf(txnSelect+` WHERE %s ORDER BY t.id DESC LIMIT $%d`, where, len(args)), args...)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	defer rows.Close()
	out := []txn{}
	for rows.Next() {
		t, err := scanTxn(rows)
		if err != nil {
			badReq(w, err.Error())
			return
		}
		out = append(out, t)
	}
	writeJSON(w, 200, out)
}

// household: every member's *shared* pockets, for the family view. Private
// pockets never leave their owner, and this endpoint is read-only — a shared
// pocket can be looked at by the others, never written to by them.
//
// The caller's own row comes first, so each member opens the view on their own
// money: Dani sees Dani → Pipit, Pipit sees Pipit → Dani.
func household(w http.ResponseWriter, r *http.Request, userID int) {
	rows, err := db.Query(`SELECT u.id, u.display_name, p.id, p.name, p.type, p.visibility,`+balanceExpr+` AS balance
		FROM inem_auth.users u
		JOIN expense.pockets p ON p.user_id=u.id AND p.visibility='shared'
		ORDER BY (u.id = $1) DESC, u.id, p.id`, userID)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	defer rows.Close()
	type memberView struct {
		UserID      int      `json:"user_id"`
		DisplayName string   `json:"display_name"`
		Pockets     []pocket `json:"pockets"`
		TotalIDR    int64    `json:"total_idr"`
	}
	members := []memberView{}
	var grand int64
	for rows.Next() {
		var uid int
		var name string
		var p pocket
		if err := rows.Scan(&uid, &name, &p.ID, &p.Name, &p.Type, &p.Visibility, &p.Balance); err != nil {
			badReq(w, err.Error())
			return
		}
		if len(members) == 0 || members[len(members)-1].UserID != uid {
			members = append(members, memberView{UserID: uid, DisplayName: name, Pockets: []pocket{}})
		}
		m := &members[len(members)-1]
		m.Pockets = append(m.Pockets, p)
		m.TotalIDR += p.Balance
		grand += p.Balance
	}
	resp := map[string]any{
		"members": members, "total_balance_idr": grand, "you": userID,
	}
	writeJSON(w, 200, resp)
}

// ---------- brain ----------

type note struct {
	ID      int64    `json:"id"`
	Title   string   `json:"title"`
	BodyMD  string   `json:"body_md"`
	Tags    []string `json:"tags"`
	Updated string   `json:"updated_at"`
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
	if in.Tags == nil {
		in.Tags = []string{} // tags is NOT NULL; a request without tags means "no tags"
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
	limit, msg := limitParam(r, 50, 200)
	if msg != "" {
		badReq(w, msg)
		return
	}
	where, args := "user_id=$1", []any{userID}
	if tag := r.URL.Query().Get("tag"); tag != "" {
		args = append(args, tag)
		where += fmt.Sprintf(" AND $%d = ANY(tags)", len(args))
	}
	args = append(args, limit)
	rows, err := db.Query(fmt.Sprintf(`SELECT id,title,body_md,tags,updated_at FROM brain.notes
		WHERE %s ORDER BY updated_at DESC LIMIT $%d`, where, len(args)), args...)
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
			Name       string  `json:"name"`
			EstGrams   int     `json:"est_grams"`
			Calories   int     `json:"calories"`
			Protein    float64 `json:"protein"`
			Carbs      float64 `json:"carbs"`
			Fat        float64 `json:"fat"`
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
		Day      string  `json:"day"`
		Calories int     `json:"calories"`
		Protein  float64 `json:"protein_g"`
		Carbs    float64 `json:"carbs_g"`
		Fat      float64 `json:"fat_g"`
		Target   *int    `json:"calorie_target,omitempty"`
	}
	var r0 rollup
	var target sql.NullInt64
	err := db.QueryRow(`SELECT to_char($1::date,'YYYY-MM-DD'), COALESCE(SUM(i.calories),0), COALESCE(SUM(i.protein),0), COALESCE(SUM(i.carbs),0), COALESCE(SUM(i.fat),0),
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

// ---------- maintenance / admin ----------
//
// Every fix a household asks for has an endpoint here — rename or retype a
// pocket, correct a note/category, drop a mistaken entry, set a calorie
// target, add a family member, wipe test data — so the agent never has to open
// psql (and never bypasses the invariants this service owns). Destructive
// calls require an explicit confirm so a stray request can't delete data.

func conflict(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusConflict, map[string]string{"error": msg})
}

func notFound(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": msg})
}

func forbidden(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusForbidden, map[string]string{"error": msg})
}

func confirmOK(r *http.Request) bool { return r.URL.Query().Get("confirm") == "true" }

// pathID reads a numeric path value ({id}) off a request.
func pathID(r *http.Request, key string) (int, error) { return strconv.Atoi(r.PathValue(key)) }

// withUserID is withUser plus a numeric {id} path value.
func withUserID(next func(w http.ResponseWriter, r *http.Request, uid, id int)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, errUID := qID(r.Header.Get("X-User-ID"))
		id, errID := pathID(r, "id")
		if errUID != nil || uid <= 0 || errID != nil || id <= 0 {
			badReq(w, "X-User-ID header and numeric {id} required")
			return
		}
		next(w, r, uid, id)
	}
}

// dateRange validates optional ?from/?to (YYYY-MM-DD); empty means no filter.
func dateRange(r *http.Request) (from, to, errMsg string) {
	q := r.URL.Query()
	from, to = q.Get("from"), q.Get("to")
	if from == "" && to == "" {
		return "", "", ""
	}
	if from == "" {
		from = to
	}
	if to == "" {
		to = from
	}
	for _, d := range []struct{ name, val string }{{"from", from}, {"to", to}} {
		if _, err := time.Parse("2006-01-02", d.val); err != nil {
			return "", "", d.name + " must be YYYY-MM-DD"
		}
	}
	if to < from {
		return "", "", "to must not be before from"
	}
	return from, to, ""
}

func limitParam(r *http.Request, def, max int) (int, string) {
	l := r.URL.Query().Get("limit")
	if l == "" {
		return def, ""
	}
	n, err := strconv.Atoi(l)
	if err != nil || n <= 0 || n > max {
		return 0, fmt.Sprintf("limit must be 1..%d", max)
	}
	return n, ""
}

// ---------- expense: pockets and entries ----------

func fetchPocket(w http.ResponseWriter, userID, id int) (pocket, bool) {
	p, err := scanPocket(db.QueryRow(pocketSQL, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		notFound(w, "pocket not found")
		return p, false
	}
	if err != nil {
		badReq(w, err.Error())
		return p, false
	}
	return p, true
}

func updatePocket(w http.ResponseWriter, r *http.Request, userID, id int) {
	var in struct {
		Name       *string `json:"name"`
		Type       *string `json:"type"`
		Opening    *int64  `json:"opening_balance_idr"`
		Visibility *string `json:"visibility"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badReq(w, err.Error())
		return
	}
	if in.Name == nil && in.Type == nil && in.Opening == nil && in.Visibility == nil {
		badReq(w, "nothing to update: send name, type, opening_balance_idr or visibility")
		return
	}
	sets := []string{}
	args := []any{userID, id}
	if in.Name != nil {
		if strings.TrimSpace(*in.Name) == "" {
			badReq(w, "name must not be empty")
			return
		}
		args = append(args, *in.Name)
		sets = append(sets, fmt.Sprintf("name=$%d", len(args)))
	}
	if in.Type != nil {
		if *in.Type != "cash" && *in.Type != "savings" && *in.Type != "investment" {
			badReq(w, "type must be cash|savings|investment")
			return
		}
		args = append(args, *in.Type)
		sets = append(sets, fmt.Sprintf("type=$%d", len(args)))
	}
	if in.Opening != nil {
		if *in.Opening < 0 {
			badReq(w, "opening_balance_idr must be >= 0")
			return
		}
		args = append(args, *in.Opening)
		sets = append(sets, fmt.Sprintf("opening_balance=$%d", len(args)))
	}
	if in.Visibility != nil {
		if *in.Visibility != "private" && *in.Visibility != "shared" {
			badReq(w, "visibility must be private|shared")
			return
		}
		args = append(args, *in.Visibility)
		sets = append(sets, fmt.Sprintf("visibility=$%d", len(args)))
	}
	res, err := db.Exec(`UPDATE expense.pockets SET `+strings.Join(sets, ", ")+` WHERE user_id=$1 AND id=$2`, args...)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			conflict(w, "pocket name exists")
			return
		}
		badReq(w, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		notFound(w, "pocket not found")
		return
	}
	if p, ok := fetchPocket(w, userID, id); ok {
		writeJSON(w, 200, p)
	}
}

func deletePocket(w http.ResponseWriter, r *http.Request, userID, id int) {
	if !confirmOK(r) {
		badReq(w, "confirm=true required")
		return
	}
	// ownership first: a pocket shared with someone else is still not theirs to
	// delete, and the answer must not reveal whether it has entries
	var owner int
	err := db.QueryRow(`SELECT user_id FROM expense.pockets WHERE id=$1 AND user_id=$2`, id, userID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		notFound(w, "pocket not found")
		return
	}
	if err != nil {
		badReq(w, err.Error())
		return
	}
	var refs int
	err = db.QueryRow(`SELECT (SELECT count(*) FROM expense.transactions WHERE pocket_id=$1)
		+ (SELECT count(*) FROM expense.transfers WHERE from_pocket=$1 OR to_pocket=$1)`, id).Scan(&refs)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	if refs > 0 {
		conflict(w, fmt.Sprintf("pocket has %d entries; delete or move them first", refs))
		return
	}
	res, err := db.Exec(`DELETE FROM expense.pockets WHERE user_id=$1 AND id=$2`, userID, id)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		notFound(w, "pocket not found")
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": n})
}

type txn struct {
	ID        int64  `json:"id"`
	PocketID  int    `json:"pocket_id"`
	Pocket    string `json:"pocket"`
	Direction string `json:"direction"`
	AmountIDR int64  `json:"amount_idr"`
	Category  string `json:"category"`
	Note      string `json:"note"`
	CreatedAt string `json:"created_at"`
}

const txnSelect = `SELECT t.id, t.pocket_id, p.name, t.direction, t.amount, t.category,
	COALESCE(t.note,''), t.created_at
	FROM expense.transactions t JOIN expense.pockets p ON p.id=t.pocket_id`

func scanTxn(s interface{ Scan(...any) error }) (txn, error) {
	var t txn
	var ts time.Time
	err := s.Scan(&t.ID, &t.PocketID, &t.Pocket, &t.Direction, &t.AmountIDR, &t.Category, &t.Note, &ts)
	t.CreatedAt = ts.Format(time.RFC3339)
	return t, err
}

func fetchTxn(w http.ResponseWriter, userID, id int) (txn, bool) {
	t, err := scanTxn(db.QueryRow(txnSelect+` WHERE t.user_id=$1 AND t.id=$2`, userID, id))
	if errors.Is(err, sql.ErrNoRows) {
		notFound(w, "transaction not found")
		return t, false
	}
	if err != nil {
		badReq(w, err.Error())
		return t, false
	}
	return t, true
}

// updateTxn: metadata only (note, category). Amounts, pocket and direction stay
// immutable so a pocket balance can never silently drift from the bank app;
// a wrong amount is corrected with a compensating entry.
func updateTxn(w http.ResponseWriter, r *http.Request, userID, id int) {
	var in struct {
		Note     *string `json:"note"`
		Category *string `json:"category"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badReq(w, err.Error())
		return
	}
	if in.Note == nil && in.Category == nil {
		badReq(w, "nothing to update: send note or category")
		return
	}
	sets := []string{}
	args := []any{userID, id}
	if in.Note != nil {
		args = append(args, *in.Note)
		sets = append(sets, fmt.Sprintf("note=$%d", len(args)))
	}
	if in.Category != nil {
		if strings.TrimSpace(*in.Category) == "" {
			badReq(w, "category must not be empty")
			return
		}
		args = append(args, *in.Category)
		sets = append(sets, fmt.Sprintf("category=$%d", len(args)))
	}
	res, err := db.Exec(`UPDATE expense.transactions SET `+strings.Join(sets, ", ")+` WHERE user_id=$1 AND id=$2`, args...)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		notFound(w, "transaction not found")
		return
	}
	if t, ok := fetchTxn(w, userID, id); ok {
		writeJSON(w, 200, t)
	}
}

func deleteTxn(w http.ResponseWriter, r *http.Request, userID, id int) {
	if !confirmOK(r) {
		badReq(w, "confirm=true required")
		return
	}
	res, err := db.Exec(`DELETE FROM expense.transactions WHERE user_id=$1 AND id=$2`, userID, id)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		notFound(w, "transaction not found")
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": n})
}

type transfer struct {
	ID         int64  `json:"id"`
	FromPocket int    `json:"from_pocket_id"`
	From       string `json:"from"`
	FromUser   string `json:"from_user"`
	ToPocket   int    `json:"to_pocket_id"`
	To         string `json:"to"`
	ToUser     string `json:"to_user"`
	AmountIDR  int64  `json:"amount_idr"`
	Note       string `json:"note"`
	CreatedAt  string `json:"created_at"`
}

// listTransfers: transfers for a period. With ?pocket_id= it serves the moves in
// or out of that pocket (its owner's or one shared with the caller). A
// counterparty pocket the caller may not see is masked: the movement stays
// visible — a shared pocket's balance must still add up — but the private
// pocket's name does not leak.
func listTransfers(w http.ResponseWriter, r *http.Request, userID int) {
	limit, msg := limitParam(r, 50, 500)
	if msg != "" {
		badReq(w, msg)
		return
	}
	from, to, msg := dateRange(r)
	if msg != "" {
		badReq(w, msg)
		return
	}
	args := []any{userID} // $1 is the caller: it also drives the masking below
	// Money that moved in or out of the caller's pockets shows up even when the
	// row was created by the other member — an incoming gift must not appear as
	// an unexplained jump in a balance.
	where := "(tr.user_id=$1 OR pf.user_id=$1 OR pt.user_id=$1)"
	if p := r.URL.Query().Get("pocket_id"); p != "" {
		pid, err := strconv.Atoi(p)
		if err != nil || pid <= 0 {
			badReq(w, "pocket_id must be a positive integer")
			return
		}
		ok, err := pocketVisibleTo(userID, pid)
		if err != nil {
			badReq(w, err.Error())
			return
		}
		if !ok {
			forbidden(w, "pocket is not yours and has not been shared")
			return
		}
		args = append(args, pid)
		where = fmt.Sprintf("(tr.from_pocket=$%d OR tr.to_pocket=$%d)", len(args), len(args))
	}
	if m := r.URL.Query().Get("month"); m != "" {
		if _, err := time.Parse("2006-01", m); err != nil {
			badReq(w, "month must be YYYY-MM")
			return
		}
		args = append(args, m)
		where += fmt.Sprintf(" AND to_char(tr.created_at,'YYYY-MM')=$%d", len(args))
	} else if from != "" {
		args = append(args, from, to)
		where += fmt.Sprintf(" AND tr.created_at::date BETWEEN $%d::date AND $%d::date", len(args)-1, len(args))
	}
	args = append(args, limit)
	rows, err := db.Query(fmt.Sprintf(`SELECT tr.id, tr.from_pocket,
		CASE WHEN pf.user_id=$1 OR pf.visibility='shared' THEN pf.name ELSE '(pribadi)' END,
		fu.display_name,
		tr.to_pocket,
		CASE WHEN pt.user_id=$1 OR pt.visibility='shared' THEN pt.name ELSE '(pribadi)' END,
		tu.display_name,
		tr.amount, COALESCE(tr.note,''), tr.created_at
		FROM expense.transfers tr
		JOIN expense.pockets pf ON pf.id=tr.from_pocket
		JOIN expense.pockets pt ON pt.id=tr.to_pocket
		JOIN inem_auth.users fu ON fu.id=pf.user_id
		JOIN inem_auth.users tu ON tu.id=pt.user_id
		WHERE %s ORDER BY tr.id DESC LIMIT $%d`, where, len(args)), args...)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	defer rows.Close()
	out := []transfer{}
	for rows.Next() {
		var t transfer
		var ts time.Time
		if err := rows.Scan(&t.ID, &t.FromPocket, &t.From, &t.FromUser, &t.ToPocket, &t.To, &t.ToUser,
			&t.AmountIDR, &t.Note, &ts); err != nil {
			badReq(w, err.Error())
			return
		}
		t.CreatedAt = ts.Format(time.RFC3339)
		out = append(out, t)
	}
	writeJSON(w, 200, out)
}

func deleteTransfer(w http.ResponseWriter, r *http.Request, userID, id int) {
	if !confirmOK(r) {
		badReq(w, "confirm=true required")
		return
	}
	res, err := db.Exec(`DELETE FROM expense.transfers WHERE user_id=$1 AND id=$2`, userID, id)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		notFound(w, "transfer not found")
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": n})
}

// ---------- expense: admin ----------

// resetData wipes one user's data. Body: {"confirm":"RESET","scope":"ledger"|"all"}.
// scope=ledger keeps pockets (and their opening balances); scope=all also drops
// pockets, notes, meals and targets — the "we were only testing" reset.
// Identity (inem_auth.users) is never touched.
func resetData(w http.ResponseWriter, r *http.Request, userID int) {
	var in struct {
		Confirm string `json:"confirm"`
		Scope   string `json:"scope"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badReq(w, err.Error())
		return
	}
	if in.Confirm != "RESET" {
		badReq(w, `confirm must be exactly "RESET"`)
		return
	}
	if in.Scope != "ledger" && in.Scope != "all" {
		badReq(w, "scope must be ledger|all")
		return
	}
	tx, err := db.Begin()
	if err != nil {
		badReq(w, err.Error())
		return
	}
	defer tx.Rollback()
	deleted, err := wipeUser(tx, userID, in.Scope == "all")
	if err != nil {
		badReq(w, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		badReq(w, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"scope": in.Scope, "deleted": deleted})
}

// wipeUser deletes one user's rows inside tx; all=false keeps pockets (with
// their opening balances), notes, meals and targets.
func wipeUser(tx *sql.Tx, userID int, all bool) (map[string]int64, error) {
	type step struct{ label, query string }
	steps := []step{
		{"transactions", `DELETE FROM expense.transactions WHERE user_id=$1`},
		{"transfers", `DELETE FROM expense.transfers WHERE user_id=$1`},
	}
	type seqDef struct{ seq, table string }
	seqs := []seqDef{
		{"expense.transactions_id_seq", "expense.transactions"},
		{"expense.transfers_id_seq", "expense.transfers"},
	}
	if all {
		steps = append(steps,
			step{"note_links", `DELETE FROM brain.note_links WHERE from_note IN (SELECT id FROM brain.notes WHERE user_id=$1)
				OR to_note IN (SELECT id FROM brain.notes WHERE user_id=$1)`},
			step{"notes", `DELETE FROM brain.notes WHERE user_id=$1`},
			step{"meal_items", `DELETE FROM nutrition.meal_items WHERE meal_id IN (SELECT id FROM nutrition.meals WHERE user_id=$1)`},
			step{"meals", `DELETE FROM nutrition.meals WHERE user_id=$1`},
			step{"daily_targets", `DELETE FROM nutrition.daily_targets WHERE user_id=$1`},
			step{"pockets", `DELETE FROM expense.pockets WHERE user_id=$1`},
		)
		seqs = append(seqs,
			seqDef{"brain.notes_id_seq", "brain.notes"},
			seqDef{"nutrition.meals_id_seq", "nutrition.meals"},
			seqDef{"nutrition.meal_items_id_seq", "nutrition.meal_items"},
			seqDef{"expense.pockets_id_seq", "expense.pockets"},
		)
	}
	deleted := map[string]int64{}
	for _, s := range steps {
		res, err := tx.Exec(s.query, userID)
		if err != nil {
			return deleted, err
		}
		n, _ := res.RowsAffected()
		deleted[s.label] = n
	}
	// Sequences are global to the table: rewinding one while another user still
	// has rows would hand out colliding ids, so only rewind an empty table.
	for _, s := range seqs {
		var rows int
		if err := tx.QueryRow("SELECT count(*) FROM " + s.table).Scan(&rows); err != nil {
			return deleted, err
		}
		if rows > 0 {
			continue
		}
		if _, err := tx.Exec("ALTER SEQUENCE " + s.seq + " RESTART WITH 1"); err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

// createUser is the bootstrap call that used to need an INSERT by hand:
// adding a family member is one POST, and pockets are created separately.
func createUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TelegramUserID int64  `json:"telegram_user_id"`
		DisplayName    string `json:"display_name"`
		Scope          string `json:"scope"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badReq(w, err.Error())
		return
	}
	if in.TelegramUserID <= 0 {
		badReq(w, "telegram_user_id must be > 0")
		return
	}
	if strings.TrimSpace(in.DisplayName) == "" {
		badReq(w, "display_name required")
		return
	}
	if in.Scope == "" {
		in.Scope = "full"
	}
	if in.Scope != "full" && in.Scope != "finance" {
		badReq(w, "scope must be full|finance")
		return
	}
	var id int
	var name, scope string
	err := db.QueryRow(`INSERT INTO inem_auth.users(telegram_user_id,display_name,scope) VALUES($1,$2,$3)
		ON CONFLICT (telegram_user_id) DO UPDATE SET display_name=EXCLUDED.display_name, scope=EXCLUDED.scope
		RETURNING id, display_name, scope`, in.TelegramUserID, in.DisplayName, in.Scope).Scan(&id, &name, &scope)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	var pockets int
	_ = db.QueryRow(`SELECT count(*) FROM expense.pockets WHERE user_id=$1`, id).Scan(&pockets)
	writeJSON(w, 201, map[string]any{"user_id": id, "display_name": name, "scope": scope, "pockets": pockets})
}

// listUsers: the household roster (who is linked, what their account covers, and
// how many pockets each has).
func listUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`SELECT u.id, u.telegram_user_id, u.display_name, u.scope,
		(SELECT count(*) FROM expense.pockets p WHERE p.user_id=u.id)
		FROM inem_auth.users u ORDER BY u.id`)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	defer rows.Close()
	type member struct {
		ID             int    `json:"id"`
		TelegramUserID int64  `json:"telegram_user_id"`
		DisplayName    string `json:"display_name"`
		Scope          string `json:"scope"`
		Pockets        int    `json:"pockets"`
	}
	out := []member{}
	for rows.Next() {
		var m member
		if err := rows.Scan(&m.ID, &m.TelegramUserID, &m.DisplayName, &m.Scope, &m.Pockets); err != nil {
			badReq(w, err.Error())
			return
		}
		out = append(out, m)
	}
	writeJSON(w, 200, out)
}

// me: the caller's own identity — what the dashboard needs to know which
// features to show without trusting anything the browser sent.
func me(w http.ResponseWriter, r *http.Request, userID int) {
	var id int
	var name, scope string
	err := db.QueryRow(`SELECT id, display_name, scope FROM inem_auth.users WHERE id=$1`, userID).
		Scan(&id, &name, &scope)
	if errors.Is(err, sql.ErrNoRows) {
		notFound(w, "user not found")
		return
	}
	if err != nil {
		badReq(w, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"user_id": id, "display_name": name, "scope": scope})
}

// deleteUser removes a member and every row they own (the scope=all wipe, then
// the identity row). The calling account cannot delete itself.
func deleteUser(w http.ResponseWriter, r *http.Request, callerID, id int) {
	if !confirmOK(r) {
		badReq(w, "confirm=true required")
		return
	}
	if id == callerID {
		badReq(w, "refusing to delete the calling account")
		return
	}
	tx, err := db.Begin()
	if err != nil {
		badReq(w, err.Error())
		return
	}
	defer tx.Rollback()
	var exists int
	err = tx.QueryRow(`SELECT 1 FROM inem_auth.users WHERE id=$1`, id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		notFound(w, "user not found")
		return
	}
	if err != nil {
		badReq(w, err.Error())
		return
	}
	deleted, err := wipeUser(tx, id, true)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	if _, err := tx.Exec(`DELETE FROM inem_auth.users WHERE id=$1`, id); err != nil {
		badReq(w, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		badReq(w, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": 1, "wiped": deleted})
}

// ---------- brain: note maintenance ----------

func updateNote(w http.ResponseWriter, r *http.Request, userID, id int) {
	var in struct {
		Title  *string   `json:"title"`
		BodyMD *string   `json:"body_md"`
		Tags   *[]string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badReq(w, err.Error())
		return
	}
	if in.Title == nil && in.BodyMD == nil && in.Tags == nil {
		badReq(w, "nothing to update: send title, body_md or tags")
		return
	}
	sets := []string{}
	args := []any{userID, id}
	if in.Title != nil {
		if strings.TrimSpace(*in.Title) == "" {
			badReq(w, "title must not be empty")
			return
		}
		args = append(args, *in.Title)
		sets = append(sets, fmt.Sprintf("title=$%d", len(args)))
	}
	if in.BodyMD != nil {
		args = append(args, *in.BodyMD)
		sets = append(sets, fmt.Sprintf("body_md=$%d", len(args)))
	}
	if in.Tags != nil {
		args = append(args, pq.Array(*in.Tags))
		sets = append(sets, fmt.Sprintf("tags=$%d", len(args)))
	}
	res, err := db.Exec(`UPDATE brain.notes SET `+strings.Join(sets, ", ")+`, updated_at=now()
		WHERE user_id=$1 AND id=$2`, args...)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		notFound(w, "note not found")
		return
	}
	var n note
	err = db.QueryRow(`SELECT id,title,body_md,tags,updated_at FROM brain.notes WHERE id=$1 AND user_id=$2`, id, userID).
		Scan(&n.ID, &n.Title, &n.BodyMD, pq.Array(&n.Tags), &n.Updated)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	writeJSON(w, 200, n)
}

func deleteNote(w http.ResponseWriter, r *http.Request, userID, id int) {
	if !confirmOK(r) {
		badReq(w, "confirm=true required")
		return
	}
	tx, err := db.Begin()
	if err != nil {
		badReq(w, err.Error())
		return
	}
	defer tx.Rollback()
	var exists int
	err = tx.QueryRow(`SELECT 1 FROM brain.notes WHERE id=$1 AND user_id=$2`, id, userID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		notFound(w, "note not found")
		return
	}
	if err != nil {
		badReq(w, err.Error())
		return
	}
	// brain.note_links has no ON DELETE CASCADE: clear the edges first.
	if _, err := tx.Exec(`DELETE FROM brain.note_links WHERE from_note=$1 OR to_note=$1`, id); err != nil {
		badReq(w, err.Error())
		return
	}
	if _, err := tx.Exec(`DELETE FROM brain.notes WHERE id=$1 AND user_id=$2`, id, userID); err != nil {
		badReq(w, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		badReq(w, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": 1})
}

// ---------- nutrition: meal maintenance + targets ----------

type mealItem struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	EstGrams   int     `json:"est_grams"`
	Calories   int     `json:"calories"`
	Protein    float64 `json:"protein_g"`
	Carbs      float64 `json:"carbs_g"`
	Fat        float64 `json:"fat_g"`
	Confidence float64 `json:"confidence"`
}

type meal struct {
	ID        int64      `json:"id"`
	PhotoRef  string     `json:"photo_ref"`
	Source    string     `json:"source"`
	Note      string     `json:"note"`
	LoggedAt  string     `json:"logged_at"`
	CreatedAt string     `json:"created_at"`
	Items     []mealItem `json:"items"`
}

// listMeals: the entries behind a day's macro rollup (default: today).
func listMeals(w http.ResponseWriter, r *http.Request, userID int) {
	day := time.Now().Format("2006-01-02")
	if d := r.URL.Query().Get("day"); d != "" {
		if _, err := time.Parse("2006-01-02", d); err != nil {
			badReq(w, "day must be YYYY-MM-DD")
			return
		}
		day = d
	}
	limit, msg := limitParam(r, 50, 200)
	if msg != "" {
		badReq(w, msg)
		return
	}
	rows, err := db.Query(`SELECT m.id, COALESCE(m.photo_ref,''), m.source, COALESCE(m.note,''), m.logged_at, m.created_at
		FROM nutrition.meals m WHERE m.user_id=$1 AND m.logged_at=$2::date ORDER BY m.id DESC LIMIT $3`, userID, day, limit)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	defer rows.Close()
	out := []meal{}
	ids := []int64{}
	for rows.Next() {
		var m meal
		var logged, created time.Time
		if err := rows.Scan(&m.ID, &m.PhotoRef, &m.Source, &m.Note, &logged, &created); err != nil {
			badReq(w, err.Error())
			return
		}
		m.LoggedAt = logged.Format("2006-01-02")
		m.CreatedAt = created.Format(time.RFC3339)
		m.Items = []mealItem{}
		out = append(out, m)
		ids = append(ids, m.ID)
	}
	if len(ids) > 0 {
		irows, err := db.Query(`SELECT meal_id, id, name, COALESCE(est_grams,0), COALESCE(calories,0),
			COALESCE(protein,0), COALESCE(carbs,0), COALESCE(fat,0), COALESCE(confidence,0)
			FROM nutrition.meal_items WHERE meal_id = ANY($1) ORDER BY id`, pq.Array(ids))
		if err != nil {
			badReq(w, err.Error())
			return
		}
		defer irows.Close()
		byMeal := map[int64][]mealItem{}
		for irows.Next() {
			var mealID int64
			var it mealItem
			if err := irows.Scan(&mealID, &it.ID, &it.Name, &it.EstGrams, &it.Calories, &it.Protein,
				&it.Carbs, &it.Fat, &it.Confidence); err != nil {
				badReq(w, err.Error())
				return
			}
			byMeal[mealID] = append(byMeal[mealID], it)
		}
		for i := range out {
			if its, ok := byMeal[out[i].ID]; ok {
				out[i].Items = its
			}
		}
	}
	writeJSON(w, 200, out)
}

func updateMeal(w http.ResponseWriter, r *http.Request, userID, id int) {
	var in struct {
		Note     *string `json:"note"`
		PhotoRef *string `json:"photo_ref"`
		LoggedAt *string `json:"logged_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badReq(w, err.Error())
		return
	}
	if in.Note == nil && in.PhotoRef == nil && in.LoggedAt == nil {
		badReq(w, "nothing to update: send note, photo_ref or logged_at")
		return
	}
	sets := []string{}
	args := []any{userID, id}
	if in.Note != nil {
		args = append(args, *in.Note)
		sets = append(sets, fmt.Sprintf("note=$%d", len(args)))
	}
	if in.PhotoRef != nil {
		args = append(args, *in.PhotoRef)
		sets = append(sets, fmt.Sprintf("photo_ref=$%d", len(args)))
	}
	if in.LoggedAt != nil {
		if _, err := time.Parse("2006-01-02", *in.LoggedAt); err != nil {
			badReq(w, "logged_at must be YYYY-MM-DD")
			return
		}
		args = append(args, *in.LoggedAt)
		sets = append(sets, fmt.Sprintf("logged_at=$%d::date", len(args)))
	}
	res, err := db.Exec(`UPDATE nutrition.meals SET `+strings.Join(sets, ", ")+` WHERE user_id=$1 AND id=$2`, args...)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		notFound(w, "meal not found")
		return
	}
	writeJSON(w, 200, map[string]any{"meal_id": id, "updated": n})
}

func deleteMeal(w http.ResponseWriter, r *http.Request, userID, id int) {
	if !confirmOK(r) {
		badReq(w, "confirm=true required")
		return
	}
	// meal_items has ON DELETE CASCADE.
	res, err := db.Exec(`DELETE FROM nutrition.meals WHERE user_id=$1 AND id=$2`, userID, id)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		notFound(w, "meal not found")
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": n})
}

type target struct {
	Day      string   `json:"day"`
	Calories *int     `json:"calorie_target,omitempty"`
	Protein  *float64 `json:"protein_target,omitempty"`
}

func targetDay(r *http.Request) (string, string) {
	day := time.Now().Format("2006-01-02")
	if d := r.URL.Query().Get("day"); d != "" {
		if _, err := time.Parse("2006-01-02", d); err != nil {
			return "", "day must be YYYY-MM-DD"
		}
		day = d
	}
	return day, ""
}

func getTarget(w http.ResponseWriter, r *http.Request, userID int) {
	day, msg := targetDay(r)
	if msg != "" {
		badReq(w, msg)
		return
	}
	var t target
	var cal sql.NullInt64
	var prot sql.NullFloat64
	err := db.QueryRow(`SELECT day::text, calorie_target, protein_target FROM nutrition.daily_targets
		WHERE user_id=$1 AND day=$2::date`, userID, day).Scan(&t.Day, &cal, &prot)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, 200, target{Day: day})
		return
	}
	if err != nil {
		badReq(w, err.Error())
		return
	}
	if cal.Valid {
		c := int(cal.Int64)
		t.Calories = &c
	}
	if prot.Valid {
		p := prot.Float64
		t.Protein = &p
	}
	writeJSON(w, 200, t)
}

func putTarget(w http.ResponseWriter, r *http.Request, userID int) {
	var in struct {
		Day      string   `json:"day"`
		Calories *int     `json:"calorie_target"`
		Protein  *float64 `json:"protein_target"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badReq(w, err.Error())
		return
	}
	day := in.Day
	if day == "" {
		day = time.Now().Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", day); err != nil {
		badReq(w, "day must be YYYY-MM-DD")
		return
	}
	if in.Calories == nil && in.Protein == nil {
		badReq(w, "nothing to set: send calorie_target and/or protein_target")
		return
	}
	if in.Calories != nil && *in.Calories <= 0 {
		badReq(w, "calorie_target must be > 0")
		return
	}
	var cal any
	if in.Calories != nil {
		cal = *in.Calories
	}
	var prot any
	if in.Protein != nil {
		prot = *in.Protein
	}
	var out target
	var c sql.NullInt64
	var p sql.NullFloat64
	err := db.QueryRow(`INSERT INTO nutrition.daily_targets(user_id,day,calorie_target,protein_target)
		VALUES($1,$2::date,$3,$4)
		ON CONFLICT (user_id,day) DO UPDATE
		SET calorie_target=COALESCE(EXCLUDED.calorie_target, nutrition.daily_targets.calorie_target),
		    protein_target=COALESCE(EXCLUDED.protein_target, nutrition.daily_targets.protein_target)
		RETURNING day::text, calorie_target, protein_target`, userID, day, cal, prot).Scan(&out.Day, &c, &p)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	if c.Valid {
		v := int(c.Int64)
		out.Calories = &v
	}
	if p.Valid {
		v := p.Float64
		out.Protein = &v
	}
	writeJSON(w, 200, out)
}

func deleteTarget(w http.ResponseWriter, r *http.Request, userID int) {
	if !confirmOK(r) {
		badReq(w, "confirm=true required")
		return
	}
	day, msg := targetDay(r)
	if msg != "" {
		badReq(w, msg)
		return
	}
	res, err := db.Exec(`DELETE FROM nutrition.daily_targets WHERE user_id=$1 AND day=$2::date`, userID, day)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		notFound(w, "no target for that day")
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": n, "day": day})
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

// route is one registered endpoint. main() registers exactly this table and the
// test suite walks the same table, so an endpoint cannot ship uncovered.
type route struct {
	Method  string
	Pattern string
	Handler http.HandlerFunc
}

func getPocket(w http.ResponseWriter, r *http.Request, userID, id int) {
	if p, ok := fetchPocket(w, userID, id); ok {
		writeJSON(w, 200, p)
	}
}

func getTxn(w http.ResponseWriter, r *http.Request, userID, id int) {
	if t, ok := fetchTxn(w, userID, id); ok {
		writeJSON(w, 200, t)
	}
}

func allRoutes() []route {
	return []route{
		{"GET", "/healthz", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, map[string]string{"status": "ok", "service": "inemd", "version": "0.1.0"})
		}},
		{"POST", "/internal/v1/handle", handlePOST},

		{"GET", "/v1/pockets", withUser(func(w http.ResponseWriter, r *http.Request, uid int) { getPockets(w, uid) })},
		{"POST", "/v1/pockets", withUser(createPocket)},
		{"GET", "/v1/pockets/{id}", withUserID(getPocket)},
		{"PATCH", "/v1/pockets/{id}", withUserID(updatePocket)},
		{"DELETE", "/v1/pockets/{id}", withUserID(deletePocket)},

		{"POST", "/v1/transactions", withUser(createTxn)},
		{"GET", "/v1/transactions", withUser(listTxns)},
		{"GET", "/v1/transactions/{id}", withUserID(getTxn)},
		{"PATCH", "/v1/transactions/{id}", withUserID(updateTxn)},
		{"DELETE", "/v1/transactions/{id}", withUserID(deleteTxn)},

		{"POST", "/v1/transfers", withUser(createTransfer)},
		{"GET", "/v1/transfers", withUser(listTransfers)},
		{"DELETE", "/v1/transfers/{id}", withUserID(deleteTransfer)},

		{"GET", "/v1/expense/summary", withUser(expenseSummary)},
		{"GET", "/v1/household", withUser(household)},

		{"POST", "/v1/admin/reset", withUser(resetData)},
		{"GET", "/v1/admin/users", listUsers},
		{"GET", "/v1/me", withUser(me)},
		{"POST", "/v1/admin/users", createUser},
		{"DELETE", "/v1/admin/users/{id}", withUserID(deleteUser)},

		{"POST", "/v1/notes", withUser(createNote)},
		{"GET", "/v1/notes", withUser(listNotes)},
		{"GET", "/v1/notes/search", withUser(searchNotes)},
		{"GET", "/v1/notes/{id}", withUserID(getNote)},
		{"PATCH", "/v1/notes/{id}", withUserID(updateNote)},
		{"DELETE", "/v1/notes/{id}", withUserID(deleteNote)},

		{"POST", "/v1/meals", withUser(createMeal)},
		{"GET", "/v1/meals", withUser(listMeals)},
		{"PATCH", "/v1/meals/{id}", withUserID(updateMeal)},
		{"DELETE", "/v1/meals/{id}", withUserID(deleteMeal)},
		{"GET", "/v1/nutrition/daily", withUser(dailyRollup)},
		{"GET", "/v1/nutrition/target", withUser(getTarget)},
		{"PUT", "/v1/nutrition/target", withUser(putTarget)},
		{"DELETE", "/v1/nutrition/target", withUser(deleteTarget)},
	}
}

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	for _, rt := range allRoutes() {
		mux.Handle(rt.Method+" "+rt.Pattern, rt.Handler)
	}
	return mux
}

func openDB(dsn string) (*sql.DB, error) {
	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(8)
	for i := 0; i < 10; i++ {
		if err = conn.Ping(); err == nil {
			return conn, nil
		}
		time.Sleep(2 * time.Second)
	}
	return nil, fmt.Errorf("db unreachable: %w", err)
}

func main() {
	dsn := os.Getenv("INEM_DSN")
	if dsn == "" {
		dsn = "postgres://inem@127.0.0.1:5432/inem?sslmode=disable"
	}
	var err error
	db, err = openDB(dsn)
	if err != nil {
		log.Fatal(err)
	}

	addr := os.Getenv("INEM_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8777"
	}
	srv := &http.Server{Addr: addr, Handler: newMux(), ReadTimeout: 15 * time.Second, WriteTimeout: 60 * time.Second}
	log.Printf("inemd listening on %s", addr)
	log.Fatal(srv.ListenAndServe())
}
