package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

// ---------- harness ----------
//
// The suite runs against a real Postgres (INEM_TEST_DSN, default inem_test) so
// the SQL, the constraints and the balance maths are exercised for real rather
// than against a mock that could drift from the schema. If the database is not
// reachable the DB-backed tests skip instead of failing.

var (
	hits      = map[string]bool{} // patterns exercised, filled by recordMux
	setupOnce sync.Once
	testConn  *sql.DB
	noDB      bool
)

func conn(t *testing.T) *sql.DB {
	t.Helper()
	setupOnce.Do(func() {
		dsn := os.Getenv("INEM_TEST_DSN")
		if dsn == "" {
			dsn = "postgres://inem@127.0.0.1:5432/inem_test?sslmode=disable"
		}
		c, err := sql.Open("postgres", dsn)
		if err != nil {
			noDB = true
			return
		}
		if err := c.Ping(); err != nil {
			fmt.Printf("test db unreachable, skipping DB tests: %v\n", err)
			noDB = true
			return
		}
		schema, err := os.ReadFile("../db/001_init.sql")
		if err != nil {
			fmt.Printf("cannot read schema: %v\n", err)
			noDB = true
			return
		}
		if _, err := c.Exec(string(schema)); err != nil {
			fmt.Printf("cannot apply schema: %v\n", err)
			noDB = true
			return
		}
		testConn = c
	})
	if noDB {
		t.Skip("no test database (set INEM_TEST_DSN)")
	}
	return testConn
}

// recordMux wires the production route table and remembers which patterns the
// suite actually hit; TestMain turns that into a coverage gate. (Go 1.22 has no
// Request.Pattern, so the recording wrapper is applied per route.)
func recordMux() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range allRoutes() {
		key := rt.Method + " " + rt.Pattern
		handler := rt.Handler
		mux.Handle(key, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits[key] = true
			handler.ServeHTTP(w, r)
		}))
	}
	return mux
}

type apiTest struct {
	t    *testing.T
	srv  *httptest.Server
	conn *sql.DB
	uid  int
}

func newAPI(t *testing.T) *apiTest {
	t.Helper()
	c := conn(t)
	if _, err := c.Exec(`TRUNCATE inem_auth.users, expense.pockets, expense.transactions, expense.transfers,
		brain.notes, brain.note_links, nutrition.meals, nutrition.meal_items, nutrition.daily_targets
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	db = c
	srv := httptest.NewServer(recordMux())
	t.Cleanup(srv.Close)
	return &apiTest{t: t, srv: srv, conn: c, uid: mkUser(t, c, 1, "Test Dani")}
}

func mkUser(t *testing.T, c *sql.DB, telegramID int64, name string) int {
	t.Helper()
	var id int
	if err := c.QueryRow(`INSERT INTO inem_auth.users(telegram_user_id, display_name)
		VALUES ($1,$2) RETURNING id`, telegramID, name).Scan(&id); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return id
}

func (a *apiTest) req(method, path string, body any, uid int, header bool) (int, []byte) {
	a.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			a.t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, a.srv.URL+path, rdr)
	if err != nil {
		a.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if header {
		req.Header.Set("X-User-ID", fmt.Sprint(uid))
	}
	resp, err := a.srv.Client().Do(req)
	if err != nil {
		a.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// call is an authenticated request as the suite's user.
func (a *apiTest) call(method, path string, body any) (int, []byte) {
	return a.req(method, path, body, a.uid, true)
}

// want asserts the status code and decodes the body (nil for an empty body).
func (a *apiTest) want(method, path string, body any, code int) any {
	a.t.Helper()
	got, raw := a.call(method, path, body)
	if got != code {
		a.t.Fatalf("%s %s: status %d, want %d — body %s", method, path, got, code, raw)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		a.t.Fatalf("%s %s: bad json: %v — %s", method, path, err, raw)
	}
	return out
}

func (a *apiTest) obj(method, path string, body any, code int) map[string]any {
	a.t.Helper()
	out := a.want(method, path, body, code)
	m, ok := out.(map[string]any)
	if !ok {
		a.t.Fatalf("%s %s: want a JSON object, got %T (%v)", method, path, out, out)
	}
	return m
}

func (a *apiTest) list(method, path string, code int) []any {
	a.t.Helper()
	out := a.want(method, path, nil, code)
	l, ok := out.([]any)
	if !ok {
		a.t.Fatalf("%s %s: want a JSON array, got %T (%v)", method, path, out, out)
	}
	return l
}

func (a *apiTest) pocket(name, typ string, opening int64) int {
	a.t.Helper()
	out := a.obj("POST", "/v1/pockets",
		map[string]any{"name": name, "type": typ, "opening_balance_idr": opening}, 201)
	return int(out["id"].(float64))
}

// balance reads one pocket's balance from the service (never computed here).
func (a *apiTest) balance(id int) int64 {
	a.t.Helper()
	for _, p := range a.list("GET", "/v1/pockets", 200) {
		pm := p.(map[string]any)
		if int(pm["id"].(float64)) == id {
			return int64(pm["balance_idr"].(float64))
		}
	}
	a.t.Fatalf("pocket %d not in /v1/pockets", id)
	return 0
}

func (a *apiTest) spend(pocketID int, direction string, amount int64, category, note string) int {
	a.t.Helper()
	out := a.obj("POST", "/v1/transactions", map[string]any{
		"pocket_id": pocketID, "direction": direction, "amount_idr": amount,
		"category": category, "note": note, "source": "manual"}, 201)
	return int(out["id"].(float64))
}

func bodyContains(raw []byte, needle string) bool {
	return bytes.Contains(bytes.ToLower(raw), bytes.ToLower([]byte(needle)))
}

// ---------- endpoints ----------

func TestHealthAndHandleContract(t *testing.T) {
	a := newAPI(t)
	health := a.obj("GET", "/healthz", nil, 200)
	if health["status"] != "ok" {
		t.Fatalf("healthz: %v", health)
	}

	a.want("POST", "/internal/v1/handle", map[string]any{},
		400) // no user_id
	a.want("POST", "/internal/v1/handle", map[string]any{"user_id": a.uid, "service": "nope"}, 400)
	ack := a.obj("POST", "/internal/v1/handle",
		map[string]any{"user_id": a.uid, "service": "expense", "trace_id": "tr-1", "text": "kopi 5rb"}, 200)
	if ack["reply_text"] == nil {
		t.Fatalf("handle ack: %v", ack)
	}
	empty := a.obj("POST", "/internal/v1/handle",
		map[string]any{"user_id": a.uid, "service": "brain"}, 200)
	if !bodyContains([]byte(empty["reply_text"].(string)), "nothing to do") {
		t.Fatalf("handle empty ack: %v", empty)
	}
}

func TestPocketLifecycleAndGuards(t *testing.T) {
	a := newAPI(t)
	cash := a.pocket("Cash", "cash", 13000)
	savings := a.pocket("Savings", "savings", 0)

	if got := a.balance(cash); got != 13000 {
		t.Fatalf("opening balance: %d", got)
	}
	if len(a.list("GET", "/v1/pockets", 200)) != 2 {
		t.Fatal("want 2 pockets")
	}

	// defaults + validation
	a.want("POST", "/v1/pockets", map[string]any{"type": "cash"}, 400)                 // no name
	a.want("POST", "/v1/pockets", map[string]any{"name": "X", "type": "gold"}, 400)    // bad type
	a.want("POST", "/v1/pockets", map[string]any{"name": "Cash", "type": "cash"}, 400) // duplicate
	def := a.obj("POST", "/v1/pockets", map[string]any{"name": "NoType"}, 201)         // type defaults to cash
	defID := int(def["id"].(float64))
	if defID == 0 {
		t.Fatal("default type pocket not created")
	}
	a.want("DELETE", fmt.Sprintf("/v1/pockets/%d?confirm=true", defID), nil, 200)

	// patch: rename + retype + opening balance
	upd := a.obj("PATCH", fmt.Sprintf("/v1/pockets/%d", cash),
		map[string]any{"name": "Cash ", "type": "savings", "opening_balance_idr": 20000}, 200)
	if upd["name"] != "Cash " || upd["type"] != "savings" {
		t.Fatalf("patch did not apply: %v", upd)
	}

	// whitespace-only name is rejected, not trimmed into place
	if got, _ := a.call("PATCH", fmt.Sprintf("/v1/pockets/%d", cash), map[string]any{"name": "   "}); got != 400 {
		t.Fatalf("blank name: status %d", got)
	}

	a.want("PATCH", fmt.Sprintf("/v1/pockets/%d", cash), map[string]any{}, 400)
	a.want("PATCH", fmt.Sprintf("/v1/pockets/%d", cash), map[string]any{"type": "gold"}, 400)
	a.want("PATCH", fmt.Sprintf("/v1/pockets/%d", cash), map[string]any{"opening_balance_idr": -1}, 400)
	a.want("PATCH", fmt.Sprintf("/v1/pockets/%d", cash), map[string]any{"name": "Savings"}, 409)
	a.want("PATCH", "/v1/pockets/9999", map[string]any{"name": "ghost"}, 404)

	// delete needs an explicit confirm, and refuses while rows reference it
	a.want("DELETE", fmt.Sprintf("/v1/pockets/%d", cash), nil, 400)
	txn := a.spend(cash, "out", 1000, "food", "test")
	_, raw := a.call("DELETE", fmt.Sprintf("/v1/pockets/%d?confirm=true", cash), nil)
	if !bodyContains(raw, "entries") {
		t.Fatalf("expected a 409 explaining the references, got %s", raw)
	}
	a.want("DELETE", fmt.Sprintf("/v1/transactions/%d?confirm=true", txn), nil, 200)
	a.want("DELETE", fmt.Sprintf("/v1/pockets/%d?confirm=true", cash), nil, 200)
	a.want("DELETE", fmt.Sprintf("/v1/pockets/%d?confirm=true", cash), nil, 404)
	if len(a.list("GET", "/v1/pockets", 200)) != 1 {
		t.Fatal("pocket count after delete")
	}
	if a.balance(savings) != 0 {
		t.Fatal("savings balance should be untouched")
	}
}

func TestBalanceIsServiceComputed(t *testing.T) {
	a := newAPI(t)
	cash := a.pocket("Cash", "cash", 100000)
	bank := a.pocket("Bank", "savings", 0)

	a.spend(cash, "out", 25000, "food", "kopi")
	a.spend(cash, "in", 5000, "refund", "kembalian")
	a.obj("POST", "/v1/transfers", map[string]any{
		"from_pocket_id": cash, "to_pocket_id": bank, "amount_idr": 50000, "note": "save"}, 201)

	if got, want := a.balance(cash), int64(100000-25000+5000-50000); got != want {
		t.Fatalf("cash balance %d, want %d", got, want)
	}
	if got, want := a.balance(bank), int64(50000); got != want {
		t.Fatalf("bank balance %d, want %d", got, want)
	}
	// deleting the transfer moves both balances back
	tr := a.list("GET", "/v1/transfers", 200)[0].(map[string]any)
	a.want("DELETE", fmt.Sprintf("/v1/transfers/%d?confirm=true", int(tr["id"].(float64))), nil, 200)
	if got := a.balance(bank); got != 0 {
		t.Fatalf("bank balance after transfer delete: %d", got)
	}
	if got := a.balance(cash); got != 80000 {
		t.Fatalf("cash balance after transfer delete: %d", got)
	}
}

func TestTransactionLifecycle(t *testing.T) {
	a := newAPI(t)
	cash := a.pocket("Cash", "cash", 200000)
	today := time.Now().Format("2006-01-02")

	id := a.spend(cash, "out", 35000, "food", "nasi goreng")

	// guards on create
	_, raw := a.call("POST", "/v1/transactions", map[string]any{
		"pocket_id": cash, "direction": "sideways", "amount_idr": 1000})
	if !bodyContains(raw, "direction") {
		t.Fatalf("direction guard: %s", raw)
	}
	_, raw = a.call("POST", "/v1/transactions", map[string]any{
		"pocket_id": cash, "direction": "out", "amount_idr": 0})
	if !bodyContains(raw, "amount_idr") {
		t.Fatalf("amount guard: %s", raw)
	}
	_, raw = a.call("POST", "/v1/transactions", map[string]any{
		"pocket_id": 9999, "direction": "out", "amount_idr": 1000})
	if !bodyContains(raw, "pocket") {
		t.Fatalf("pocket guard: %s", raw)
	}

	// list + filters
	if got := len(a.list("GET", "/v1/transactions", 200)); got != 1 {
		t.Fatalf("list: %d rows", got)
	}
	if got := len(a.list("GET", "/v1/transactions?category=food", 200)); got != 1 {
		t.Fatalf("category filter: %d rows", got)
	}
	if got := len(a.list("GET", "/v1/transactions?category=transport", 200)); got != 0 {
		t.Fatalf("category filter should exclude: %d rows", got)
	}
	if got := len(a.list("GET", "/v1/transactions?direction=in", 200)); got != 0 {
		t.Fatalf("direction filter: %d rows", got)
	}
	if got := len(a.list("GET", "/v1/transactions?from="+today+"&to="+today, 200)); got != 1 {
		t.Fatalf("date filter: %d rows", got)
	}
	a.want("GET", "/v1/transactions?from=2026-01-02&to=2026-01-01", nil, 400)
	a.want("GET", "/v1/transactions?from=nope", nil, 400)
	a.want("GET", "/v1/transactions?limit=0", nil, 400)
	a.want("GET", "/v1/transactions?limit=501", nil, 400)

	one := a.obj("GET", fmt.Sprintf("/v1/transactions/%d", id), nil, 200)
	if one["note"] != "nasi goreng" || int(one["id"].(float64)) != id {
		t.Fatalf("get txn: %v", one)
	}
	a.want("GET", "/v1/transactions/9999", nil, 404)

	fixed := a.obj("PATCH", fmt.Sprintf("/v1/transactions/%d", id),
		map[string]any{"note": "nasi goreng + telur", "category": "food"}, 200)
	if fixed["note"] != "nasi goreng + telur" {
		t.Fatalf("patch note: %v", fixed)
	}
	a.want("PATCH", fmt.Sprintf("/v1/transactions/%d", id), map[string]any{}, 400)
	a.want("PATCH", fmt.Sprintf("/v1/transactions/%d", id), map[string]any{"category": "  "}, 400)
	a.want("PATCH", "/v1/transactions/9999", map[string]any{"note": "x"}, 404)
	// amounts stay immutable, and the lie is refused rather than silently dropped
	_, raw = a.call("PATCH", fmt.Sprintf("/v1/transactions/%d", id), map[string]any{"amount_idr": 99000})
	if !bodyContains(raw, "nothing to update") {
		t.Fatalf("amount must not be editable: %s", raw)
	}
	if got := a.balance(cash); got != 165000 {
		t.Fatalf("balance changed by a metadata edit: %d", got)
	}

	a.want("DELETE", fmt.Sprintf("/v1/transactions/%d", id), nil, 400)
	a.want("DELETE", fmt.Sprintf("/v1/transactions/%d?confirm=true", id), nil, 200)
	a.want("DELETE", fmt.Sprintf("/v1/transactions/%d?confirm=true", id), nil, 404)
	if got := a.balance(cash); got != 200000 {
		t.Fatalf("balance after delete: %d", got)
	}
}

func TestTransferLifecycle(t *testing.T) {
	a := newAPI(t)
	cash := a.pocket("Cash", "cash", 100000)
	bank := a.pocket("Bank", "savings", 0)
	month := time.Now().Format("2006-01")
	today := time.Now().Format("2006-01-02")

	a.want("POST", "/v1/transfers", map[string]any{"from_pocket_id": cash, "to_pocket_id": cash, "amount_idr": 1000}, 400)
	a.want("POST", "/v1/transfers", map[string]any{"from_pocket_id": cash, "to_pocket_id": bank, "amount_idr": 0}, 400)
	a.want("POST", "/v1/transfers", map[string]any{"from_pocket_id": 9999, "to_pocket_id": bank, "amount_idr": 100}, 400)

	tr := a.obj("POST", "/v1/transfers", map[string]any{
		"from_pocket_id": cash, "to_pocket_id": bank, "amount_idr": 40000, "note": "monthly save"}, 201)
	id := int(tr["id"].(float64))

	row := a.list("GET", "/v1/transfers", 200)[0].(map[string]any)
	if row["from"] != "Cash" || row["to"] != "Bank" || int64(row["amount_idr"].(float64)) != 40000 {
		t.Fatalf("transfer row: %v", row)
	}
	if got := len(a.list("GET", "/v1/transfers?month="+month, 200)); got != 1 {
		t.Fatalf("month filter: %d", got)
	}
	if got := len(a.list("GET", "/v1/transfers?from="+today+"&to="+today, 200)); got != 1 {
		t.Fatalf("range filter: %d", got)
	}
	if got := len(a.list("GET", "/v1/transfers?from=2020-01-01&to=2020-01-02", 200)); got != 0 {
		t.Fatalf("empty range: %d", got)
	}
	a.want("GET", "/v1/transfers?month=nope", nil, 400)
	a.want("GET", "/v1/transfers?limit=0", nil, 400)

	a.want("DELETE", fmt.Sprintf("/v1/transfers/%d", id), nil, 400)
	a.want("DELETE", fmt.Sprintf("/v1/transfers/%d?confirm=true", id), nil, 200)
	a.want("DELETE", fmt.Sprintf("/v1/transfers/%d?confirm=true", id), nil, 404)
}

func TestSummaryRollsUpPeriods(t *testing.T) {
	a := newAPI(t)
	cash := a.pocket("Cash", "cash", 100000)
	today := time.Now().Format("2006-01-02")
	month := time.Now().Format("2006-01")

	a.spend(cash, "out", 25000, "food", "kopi")
	a.spend(cash, "out", 10000, "transport", "ojek")
	a.spend(cash, "in", 5000, "refund", "kembalian")

	cur := a.obj("GET", "/v1/expense/summary", nil, 200)
	if int64(cur["total_out_idr"].(float64)) != 35000 || int64(cur["total_in_idr"].(float64)) != 5000 {
		t.Fatalf("current-month totals: %v", cur)
	}
	if cur["month"] != month {
		t.Fatalf("month label: %v", cur["month"])
	}
	byCat := map[string]int64{}
	for _, c := range cur["by_category"].([]any) {
		cm := c.(map[string]any)
		if cm["direction"] == "out" {
			byCat[cm["category"].(string)] = int64(cm["total_idr"].(float64))
		}
	}
	if byCat["food"] != 25000 || byCat["transport"] != 10000 {
		t.Fatalf("by_category: %v", byCat)
	}

	rangeOut := a.obj("GET", "/v1/expense/summary?from="+today+"&to="+today, nil, 200)
	if int64(rangeOut["total_out_idr"].(float64)) != 35000 {
		t.Fatalf("range totals: %v", rangeOut)
	}
	if rangeOut["from"] != today {
		t.Fatalf("range label: %v", rangeOut)
	}

	empty := a.obj("GET", "/v1/expense/summary?month=2020-01", nil, 200)
	if len(empty["by_category"].([]any)) != 0 {
		t.Fatalf("empty month should roll up to nothing: %v", empty)
	}
	a.want("GET", "/v1/expense/summary?month=2020-13", nil, 400)
	a.want("GET", "/v1/expense/summary?from=2026-01-02&to=2026-01-01", nil, 400)
	a.want("GET", "/v1/expense/summary?from=nope&to=nope", nil, 400)
}

func TestNotesCrudSearchAndLinks(t *testing.T) {
	a := newAPI(t)
	first := a.obj("POST", "/v1/notes",
		map[string]any{"title": "Wifi bill", "body_md": "Pay on the 5th, 385rb.", "tags": []string{"bills", "home"}}, 201)
	second := a.obj("POST", "/v1/notes",
		map[string]any{"title": "Pocket rule", "body_md": "Set aside 20% every payday.", "tags": []string{"finance"}}, 201)
	a.want("POST", "/v1/notes", map[string]any{"body_md": "no title"}, 400)

	firstID := int(first["id"].(float64))
	secondID := int(second["id"].(float64))

	if got := len(a.list("GET", "/v1/notes", 200)); got != 2 {
		t.Fatalf("list notes: %d", got)
	}
	if got := len(a.list("GET", "/v1/notes?tag=finance", 200)); got != 1 {
		t.Fatalf("tag filter: %d", got)
	}
	if got := len(a.list("GET", "/v1/notes?limit=1", 200)); got != 1 {
		t.Fatalf("limit: %d", got)
	}
	a.want("GET", "/v1/notes?limit=1000", nil, 400)
	if got := len(a.list("GET", "/v1/notes/search?q=payday", 200)); got != 1 {
		t.Fatalf("search: %d", got)
	}
	a.want("GET", "/v1/notes/search", nil, 400)
	if got := len(a.list("GET", "/v1/notes/search?q=nothingmatchesthis", 200)); got != 0 {
		t.Fatalf("search miss: %d", got)
	}

	one := a.obj("GET", fmt.Sprintf("/v1/notes/%d", firstID), nil, 200)
	if one["title"] != "Wifi bill" || one["body_md"] != "Pay on the 5th, 385rb." {
		t.Fatalf("get note: %v", one)
	}
	a.want("GET", "/v1/notes/9999", nil, 400)

	upd := a.obj("PATCH", fmt.Sprintf("/v1/notes/%d", firstID),
		map[string]any{"title": "Wifi bill (house)", "tags": []string{"bills"}}, 200)
	if upd["title"] != "Wifi bill (house)" || len(upd["tags"].([]any)) != 1 {
		t.Fatalf("patch note: %v", upd)
	}
	a.want("PATCH", fmt.Sprintf("/v1/notes/%d", firstID), map[string]any{}, 400)
	a.want("PATCH", fmt.Sprintf("/v1/notes/%d", firstID), map[string]any{"title": "  "}, 400)
	a.want("PATCH", "/v1/notes/9999", map[string]any{"title": "x"}, 404)

	// note_links has no ON DELETE CASCADE: deleting a linked note must not 500
	if _, err := a.conn.Exec(`INSERT INTO brain.note_links(from_note,to_note) VALUES ($1,$2)`, firstID, secondID); err != nil {
		t.Fatalf("link insert: %v", err)
	}
	a.want("DELETE", fmt.Sprintf("/v1/notes/%d", firstID), nil, 400)
	a.want("DELETE", fmt.Sprintf("/v1/notes/%d?confirm=true", firstID), nil, 200)
	var links int
	if err := a.conn.QueryRow(`SELECT count(*) FROM brain.note_links WHERE from_note=$1 OR to_note=$1`, firstID).Scan(&links); err != nil {
		t.Fatalf("link count: %v", err)
	}
	if links != 0 {
		t.Fatalf("dangling note_links: %d", links)
	}
	a.want("DELETE", fmt.Sprintf("/v1/notes/%d?confirm=true", firstID), nil, 404)
	if got := len(a.list("GET", "/v1/notes", 200)); got != 1 {
		t.Fatalf("notes after delete: %d", got)
	}
}

func TestMealsMacrosAndTargets(t *testing.T) {
	a := newAPI(t)
	meal := a.obj("POST", "/v1/meals", map[string]any{
		"source": "manual", "note": "lunch",
		"items": []map[string]any{
			{"name": "nasi goreng", "est_grams": 350, "calories": 620, "protein": 18.5, "carbs": 85, "fat": 21, "confidence": 0.8},
			{"name": "telur", "est_grams": 60, "calories": 90, "protein": 7, "carbs": 1, "fat": 6, "confidence": 0.9},
		}}, 201)
	mealID := int(meal["meal_id"].(float64))
	if int(meal["items"].(float64)) != 2 {
		t.Fatalf("meal items: %v", meal)
	}
	a.want("POST", "/v1/meals", map[string]any{"source": "telepathy"}, 400)
	a.want("POST", "/v1/meals", map[string]any{"items": []any{}}, 201) // an empty day is allowed

	today := time.Now().Format("2006-01-02")
	meals := a.list("GET", "/v1/meals", 200)
	if len(meals) != 2 {
		t.Fatalf("list meals: %d", len(meals))
	}
	first := map[string]any{}
	for _, m := range meals {
		mm := m.(map[string]any)
		if int(mm["id"].(float64)) == mealID {
			first = mm
		}
	}
	if len(first["items"].([]any)) != 2 {
		t.Fatalf("meal items in list: %v", meals)
	}
	if got := len(a.list("GET", "/v1/meals?day="+today, 200)); got != 2 {
		t.Fatalf("day filter: %d", got)
	}
	if got := len(a.list("GET", "/v1/meals?day=2020-01-01", 200)); got != 0 {
		t.Fatalf("empty day: %d", got)
	}
	a.want("GET", "/v1/meals?day=nope", nil, 400)

	r0 := a.obj("GET", "/v1/nutrition/daily", nil, 200)
	if int(r0["calories"].(float64)) != 710 || r0["day"] != today {
		t.Fatalf("daily rollup: %v", r0)
	}
	if r0["protein_g"].(float64) != 25.5 {
		t.Fatalf("protein rollup: %v", r0)
	}

	// targets: read, upsert, clear
	a.want("GET", "/v1/nutrition/target?day=nope", nil, 400)
	empty := a.obj("GET", "/v1/nutrition/target", nil, 200)
	if empty["calorie_target"] != nil {
		t.Fatalf("unset target: %v", empty)
	}
	set := a.obj("PUT", "/v1/nutrition/target",
		map[string]any{"calorie_target": 2100, "protein_target": 120}, 200)
	if int(set["calorie_target"].(float64)) != 2100 {
		t.Fatalf("set target: %v", set)
	}
	a.want("PUT", "/v1/nutrition/target", map[string]any{}, 400)
	a.want("PUT", "/v1/nutrition/target", map[string]any{"calorie_target": 0}, 400)
	a.want("PUT", "/v1/nutrition/target", map[string]any{"day": "nope", "calorie_target": 2000}, 400)
	patch := a.obj("PUT", "/v1/nutrition/target", map[string]any{"protein_target": 130}, 200)
	if int(patch["calorie_target"].(float64)) != 2100 || patch["protein_target"].(float64) != 130 {
		t.Fatalf("partial target update kept the other field? %v", patch)
	}
	if got := a.obj("GET", "/v1/nutrition/daily", nil, 200); int(got["calorie_target"].(float64)) != 2100 {
		t.Fatalf("daily should expose the target: %v", got)
	}
	a.want("DELETE", "/v1/nutrition/target", nil, 400)
	a.want("DELETE", "/v1/nutrition/target?confirm=true", nil, 200)
	a.want("DELETE", "/v1/nutrition/target?confirm=true", nil, 404)

	upd := a.obj("PATCH", fmt.Sprintf("/v1/meals/%d", mealID),
		map[string]any{"note": "lunch (estimate)", "logged_at": today}, 200)
	if int(upd["updated"].(float64)) != 1 {
		t.Fatalf("meal patch: %v", upd)
	}
	a.want("PATCH", fmt.Sprintf("/v1/meals/%d", mealID), map[string]any{}, 400)
	a.want("PATCH", fmt.Sprintf("/v1/meals/%d", mealID), map[string]any{"logged_at": "nope"}, 400)
	a.want("PATCH", "/v1/meals/9999", map[string]any{"note": "x"}, 404)
	a.want("DELETE", fmt.Sprintf("/v1/meals/%d", mealID), nil, 400)
	a.want("DELETE", fmt.Sprintf("/v1/meals/%d?confirm=true", mealID), nil, 200)
	var items int
	if err := a.conn.QueryRow(`SELECT count(*) FROM nutrition.meal_items WHERE meal_id=$1`, mealID).Scan(&items); err != nil {
		t.Fatalf("item count: %v", err)
	}
	if items != 0 {
		t.Fatalf("meal_items not cascaded: %d", items)
	}
	a.want("DELETE", fmt.Sprintf("/v1/meals/%d?confirm=true", mealID), nil, 404)
}

func TestUserAdminRosterAndRemoval(t *testing.T) {
	a := newAPI(t)
	roster := a.list("GET", "/v1/admin/users", 200)
	if len(roster) != 1 {
		t.Fatalf("roster: %v", roster)
	}
	// upsert: same telegram id twice must not duplicate the member
	spouse := a.obj("POST", "/v1/admin/users",
		map[string]any{"telegram_user_id": 555000111, "display_name": "Istri"}, 201)
	again := a.obj("POST", "/v1/admin/users",
		map[string]any{"telegram_user_id": 555000111, "display_name": "Istri (Seabank)"}, 201)
	if spouse["user_id"] != again["user_id"] {
		t.Fatalf("telegram id must map to one member: %v vs %v", spouse, again)
	}
	if got := len(a.list("GET", "/v1/admin/users", 200)); got != 2 {
		t.Fatalf("roster after upsert: %d", got)
	}
	a.want("POST", "/v1/admin/users", map[string]any{"telegram_user_id": 0, "display_name": "x"}, 400)
	a.want("POST", "/v1/admin/users", map[string]any{"telegram_user_id": 42, "display_name": " "}, 400)

	spouseID := int(spouse["user_id"].(float64))
	// the spouse gets her own pocket; removing her must take it along
	a.req("POST", "/v1/pockets",
		map[string]any{"name": "Seabank", "type": "savings", "opening_balance_idr": 700000}, spouseID, true)
	var spousePocket int
	if err := a.conn.QueryRow(`SELECT id FROM expense.pockets WHERE user_id=$1`, spouseID).Scan(&spousePocket); err != nil {
		t.Fatalf("spouse pocket: %v", err)
	}
	if _, raw := a.req("GET", "/v1/pockets", nil, spouseID, true); !bodyContains(raw, "Seabank") {
		t.Fatalf("spouse pocket missing: %s", raw)
	}

	a.want("DELETE", fmt.Sprintf("/v1/admin/users/%d", spouseID), nil, 400)
	a.want("DELETE", fmt.Sprintf("/v1/admin/users/%d?confirm=true", a.uid), nil, 400) // self
	a.want("DELETE", "/v1/admin/users/9999?confirm=true", nil, 404)
	a.want("DELETE", fmt.Sprintf("/v1/admin/users/%d?confirm=true", spouseID), nil, 200)
	if got := len(a.list("GET", "/v1/admin/users", 200)); got != 1 {
		t.Fatalf("roster after removal: %d", got)
	}
	var left int
	if err := a.conn.QueryRow(`SELECT count(*) FROM expense.pockets WHERE user_id=$1`, spouseID).Scan(&left); err != nil {
		t.Fatalf("pocket count: %v", err)
	}
	if left != 0 {
		t.Fatalf("member rows not wiped: %d pockets left", left)
	}
}

func TestResetScopesKeepOtherMembers(t *testing.T) {
	a := newAPI(t)
	other := mkUser(t, a.conn, 2, "Istri")
	a.pocket("Cash", "cash", 50000)
	pocketID := int(a.list("GET", "/v1/pockets", 200)[0].(map[string]any)["id"].(float64))
	a.spend(pocketID, "out", 5000, "food", "kopi")
	a.obj("POST", "/v1/notes", map[string]any{"title": "n1", "body_md": "b"}, 201)
	a.obj("POST", "/v1/meals", map[string]any{"items": []map[string]any{{"name": "nasi", "calories": 300}}}, 201)

	// another member's data, to prove the wipe is scoped to one user
	var otherPocket int
	if err := a.conn.QueryRow(`INSERT INTO expense.pockets(user_id,name,type,opening_balance)
		VALUES ($1,'Cash','cash',999) RETURNING id`, other).Scan(&otherPocket); err != nil {
		t.Fatalf("other pocket: %v", err)
	}

	a.want("POST", "/v1/admin/reset", map[string]any{"confirm": "nope", "scope": "all"}, 400)
	a.want("POST", "/v1/admin/reset", map[string]any{"confirm": "RESET", "scope": "nope"}, 400)
	a.want("POST", "/v1/admin/reset", map[string]any{"confirm": "RESET", "scope": "banana"}, 400)

	ledger := a.obj("POST", "/v1/admin/reset", map[string]any{"confirm": "RESET", "scope": "ledger"}, 200)
	del := ledger["deleted"].(map[string]any)
	if int(del["transactions"].(float64)) != 1 {
		t.Fatalf("ledger reset counts: %v", del)
	}
	if got := len(a.list("GET", "/v1/pockets", 200)); got != 1 {
		t.Fatalf("scope=ledger must keep pockets: %d", got)
	}
	if got := len(a.list("GET", "/v1/notes", 200)); got != 1 {
		t.Fatalf("scope=ledger must keep notes: %d", got)
	}

	all := a.obj("POST", "/v1/admin/reset", map[string]any{"confirm": "RESET", "scope": "all"}, 200)
	delAll := all["deleted"].(map[string]any)
	if int(delAll["pockets"].(float64)) != 1 || int(delAll["notes"].(float64)) != 1 || int(delAll["meals"].(float64)) != 1 {
		t.Fatalf("scope=all counts: %v", delAll)
	}
	if got := len(a.list("GET", "/v1/pockets", 200)); got != 0 {
		t.Fatalf("pockets after scope=all: %d", got)
	}

	// regression: the wipe must not rewind a sequence other members still use
	// (that handed out colliding ids and made the next INSERT fail)
	if code, _ := a.call("POST", "/v1/pockets",
		map[string]any{"name": "New Cash", "type": "cash", "opening_balance_idr": 1000}); code != 201 {
		t.Fatalf("insert after reset should succeed, got %d", code)
	}
	var otherBalance int64
	if err := a.conn.QueryRow(`SELECT opening_balance FROM expense.pockets WHERE id=$1`, otherPocket).Scan(&otherBalance); err != nil {
		t.Fatalf("other member's pocket was touched: %v", err)
	}
	if otherBalance != 999 {
		t.Fatalf("other member's pocket changed: %d", otherBalance)
	}
}

func TestAuthHeaderAndCrossUserIsolation(t *testing.T) {
	a := newAPI(t)
	other := mkUser(t, a.conn, 2, "Istri")
	mine := a.pocket("Cash", "cash", 100000)
	txn := a.spend(mine, "out", 1000, "food", "mine")
	note := a.obj("POST", "/v1/notes", map[string]any{"title": "mine", "body_md": "secret"}, 201)

	// missing or bogus X-User-ID is refused on every guarded surface
	if code, _ := a.req("GET", "/v1/pockets", nil, 0, false); code != 400 {
		t.Fatalf("no header: %d", code)
	}
	if code, _ := a.req("GET", "/v1/pockets", nil, -1, true); code != 400 {
		t.Fatalf("negative id: %d", code)
	}
	if code, _ := a.req("GET", "/v1/notes/abc", nil, a.uid, true); code != 400 {
		t.Fatalf("non-numeric path id: %d", code)
	}

	// an empty pocket of mine, to prove the other member cannot delete it
	empty := a.pocket("Empty", "cash", 0)

	// the other member sees nothing of mine and cannot touch it
	if code, raw := a.req("GET", "/v1/pockets", nil, other, true); code != 200 || bodyContains(raw, "Cash") {
		t.Fatalf("cross-user pocket leak: %d %s", code, raw)
	}
	if code, raw := a.req("GET", "/v1/transactions", nil, other, true); code != 200 || bodyContains(raw, "mine") {
		t.Fatalf("cross-user txn leak: %d %s", code, raw)
	}
	if code, _ := a.req("GET", fmt.Sprintf("/v1/notes/%d", int(note["id"].(float64))), nil, other, true); code == 200 {
		t.Fatal("cross-user note read allowed")
	}
	if code, _ := a.req("PATCH", fmt.Sprintf("/v1/transactions/%d", txn), map[string]any{"note": "hacked"}, other, true); code != 404 {
		t.Fatalf("cross-user txn patch: %d", code)
	}
	if code, _ := a.req("DELETE", fmt.Sprintf("/v1/pockets/%d?confirm=true", empty), nil, other, true); code != 404 {
		t.Fatalf("cross-user pocket delete: %d", code)
	}
	if code, _ := a.req("PATCH", fmt.Sprintf("/v1/pockets/%d", empty), map[string]any{"name": "Hacked"}, other, true); code != 404 {
		t.Fatalf("cross-user pocket patch: %d", code)
	}
	if code, _ := a.req("GET", "/v1/expense/summary", nil, other, true); code != 200 {
		t.Fatalf("cross-user summary: %d", code)
	}
	a.want("GET", fmt.Sprintf("/v1/notes/%d", int(note["id"].(float64))), nil, 200)
	if got := a.balance(mine); got != 99000 {
		t.Fatalf("my balance moved: %d", got)
	}
	if got := a.balance(empty); got != 0 {
		t.Fatalf("my empty pocket moved: %d", got)
	}
}
