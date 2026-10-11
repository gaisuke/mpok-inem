package main

// Forecast: what next month is likely to cost, built from what actually happened.
//
// The question this answers is not "what did you spend" but "which of it will
// happen again". Two things make that answerable from the ledger itself:
//
//  1. a category that shows up month after month is a habit, and its median is a
//     better guess than any single month (the mean would follow a spike);
//  2. one entry that dwarfs its own category is an event, not a habit — the
//     phone bought this month will not be bought again next month.
//
// A category seen in one of three months is amortised rather than dropped: Rp100
// every third month is Rp33 a month, and pretending it is zero is how a budget
// lies to you.
//
// The forecast therefore has three parts: the habit (median, amortised), the
// events that are excluded and shown by name, and the plans already written down
// (schedules), which are commitments rather than guesses.

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type forecastCategory struct {
	Category    string `json:"category"`
	Direction   string `json:"direction"`
	MonthsSeen  int    `json:"months_seen"`      // in how many history months it appeared
	Window      int    `json:"window"`           // how many months were examined
	MedianMonth int64  `json:"median_month_idr"` // median of the months it appeared in
	ForecastIDR int64  `json:"forecast_idr"`     // median × months_seen / window
	Pattern     string `json:"pattern"`          // tetap | rutin | jarang | sekali
	Entries     int    `json:"entries"`
}

type forecastEvent struct {
	ID       int64  `json:"id"`
	Date     string `json:"date"`
	Category string `json:"category"`
	Amount   int64  `json:"amount_idr"`
	Note     string `json:"note"`
	Pocket   string `json:"pocket"`
	Why      string `json:"why"`
}

type forecastMember struct {
	UserID        int                `json:"user_id"`
	Name          string             `json:"name"`
	Me            bool               `json:"me"`
	HistoryMonths []string           `json:"history_months"`
	SpendForecast int64              `json:"spend_forecast_idr"`
	SpendMedian   int64              `json:"spend_median_idr"` // median monthly spend in the window
	EventsTotal   int64              `json:"one_off_total_idr"`
	Categories    []forecastCategory `json:"categories"`
	Events        []forecastEvent    `json:"one_off"`
	Hints         []string           `json:"hints"`
}

// monthList returns the n months before `month` (YYYY-MM), oldest first.
func monthList(month string, n int) []string {
	t, err := time.Parse("2006-01", month)
	if err != nil {
		return nil
	}
	out := make([]string, 0, n)
	for i := n; i >= 1; i-- {
		out = append(out, t.AddDate(0, -i, 0).Format("2006-01"))
	}
	return out
}

func nextMonth(t time.Time) string { return t.AddDate(0, 1, 0).Format("2006-01") }

func median(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]int64(nil), xs...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	mid := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[mid]
	}
	return (cp[mid-1] + cp[mid]) / 2
}

// oneOff reports whether a single entry is an event rather than a habit. It has
// to clear both bars: big in absolute terms (a 300rb yearly domain is not a
// phone), and far above what this person normally spends in one go.
func oneOff(amount, typical int64) bool {
	return amount >= 1000000 && amount >= 3*typical
}

// baseline is the yardstick an entry is judged against. A category with a real
// history judges its own entries; a category seen once (a phone bought once) must
// not judge itself — that is how a spike disguises itself as the pattern.
func baseline(catAmounts, allAmounts []int64) int64 {
	if len(catAmounts) >= 3 {
		if m := median(catAmounts); m > 0 {
			return m
		}
	}
	if m := median(allAmounts); m > 0 {
		return m
	}
	return median(catAmounts)
}

// expenseForecast: GET /v1/expense/forecast?month=YYYY-MM&months=3
//
// `month` defaults to next month, `months` to 3 (1..6) — the completed months the
// guess is built from. Every household member is included: the caller's own data
// in full, another member's through the pockets they have shared, exactly as the
// ledger itself is served.
func expenseForecast(w http.ResponseWriter, r *http.Request, userID int) {
	q := r.URL.Query()
	month := q.Get("month")
	if month == "" {
		month = nextMonth(time.Now())
	}
	if _, err := time.Parse("2006-01", month); err != nil {
		badReq(w, "month must be YYYY-MM")
		return
	}
	window := 3
	if v := q.Get("months"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 6 {
			badReq(w, "months must be 1..6")
			return
		}
		window = n
	}
	history := monthList(month, window)
	from := history[0] + "-01"
	to := month + "-01" // exclusive

	// who: the caller first, so the page opens on their own money
	rows, err := db.Query(`SELECT id, display_name FROM inem_auth.users
		ORDER BY (id = $1) DESC, id`, userID)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	type member struct {
		id   int
		name string
	}
	members := []member{}
	for rows.Next() {
		var m member
		if err := rows.Scan(&m.id, &m.name); err != nil {
			rows.Close()
			badReq(w, err.Error())
			return
		}
		members = append(members, m)
	}
	rows.Close()

	// one pass over every entry in the window, with the pocket name and whether
	// the caller is allowed to see it
	type entry struct {
		id, amount                                  int64
		userID                                      int
		category, direction, note, pocket, monthStr string
		date                                        string
		visible                                     bool
	}
	entries := []entry{}
	erows, err := db.Query(`
		SELECT t.id, t.user_id, p.name, t.direction, t.amount, t.category,
		       COALESCE(t.note,''), to_char(t.created_at,'YYYY-MM'), t.created_at::date::text,
		       (t.user_id = $1 OR p.visibility = 'shared') AS visible
		FROM expense.transactions t
		JOIN expense.pockets p ON p.id = t.pocket_id
		WHERE t.created_at >= $2::date AND t.created_at < $3::date
		  AND (t.user_id = $1 OR p.visibility = 'shared')
		ORDER BY t.created_at`, userID, from, to)
	if err != nil {
		badReq(w, err.Error())
		return
	}
	for erows.Next() {
		var e entry
		if err := erows.Scan(&e.id, &e.userID, &e.pocket, &e.direction, &e.amount,
			&e.category, &e.note, &e.monthStr, &e.date, &e.visible); err != nil {
			erows.Close()
			badReq(w, err.Error())
			return
		}
		entries = append(entries, e)
	}
	erows.Close()

	out := []forecastMember{}
	for _, m := range members {
		var mine []entry
		for _, e := range entries {
			if e.userID == m.id {
				mine = append(mine, e)
			}
		}
		fm := forecastMember{UserID: m.id, Name: m.name, Me: m.id == userID,
			HistoryMonths: history, Categories: []forecastCategory{}, Events: []forecastEvent{}, Hints: []string{}}

		// per category: what each history month cost (one-offs removed), plus the
		// entries that are events rather than habits
		type acc struct {
			perMonth map[string]int64
			months   map[string]bool
			entries  int
		}
		byCat := map[string]*acc{}
		amountsByCat := map[string][]int64{}
		var allAmounts []int64
		for _, e := range mine {
			if e.direction != "out" {
				continue
			}
			amountsByCat[e.category] = append(amountsByCat[e.category], e.amount)
			allAmounts = append(allAmounts, e.amount)
		}
		for _, e := range mine {
			if e.direction != "out" {
				continue
			}
			typical := baseline(amountsByCat[e.category], allAmounts)
			if oneOff(e.amount, typical) {
				fm.EventsTotal += e.amount
				fm.Events = append(fm.Events, forecastEvent{ID: e.id, Date: e.date,
					Category: e.category, Amount: e.amount, Note: e.note, Pocket: e.pocket,
					Why: fmt.Sprintf("%d× lebih besar dari pola transaksi biasanya (%s) — kemungkinan sekali ini saja",
						e.amount/max1(typical), rupiahText(typical))})
				continue
			}
			a := byCat[e.category]
			if a == nil {
				a = &acc{perMonth: map[string]int64{}, months: map[string]bool{}}
				byCat[e.category] = a
			}
			a.perMonth[e.monthStr] += e.amount
			a.months[e.monthStr] = true
			a.entries++
		}
		var monthlyTotals []int64
		for _, h := range history {
			var sum int64
			for _, a := range byCat {
				sum += a.perMonth[h]
			}
			monthlyTotals = append(monthlyTotals, sum)
		}
		fm.SpendMedian = median(monthlyTotals)

		cats := []forecastCategory{}
		for cat, a := range byCat {
			vals := []int64{}
			for _, h := range history {
				if a.months[h] {
					vals = append(vals, a.perMonth[h])
				}
			}
			seen := len(vals)
			med := median(vals)
			pattern := "jarang"
			switch {
			case seen >= len(history) && seen > 1:
				pattern = "rutin"
			case seen > 1:
				pattern = "rutin"
			case seen == 1:
				pattern = "jarang"
			}
			fc := forecastCategory{Category: cat, Direction: "out", MonthsSeen: seen,
				Window: len(history), MedianMonth: med,
				ForecastIDR: med * int64(seen) / int64(len(history)),
				Pattern:     pattern, Entries: a.entries}
			fm.SpendForecast += fc.ForecastIDR
			cats = append(cats, fc)
		}
		sort.Slice(cats, func(i, j int) bool { return cats[i].ForecastIDR > cats[j].ForecastIDR })
		fm.Categories = cats
		sort.Slice(fm.Events, func(i, j int) bool { return fm.Events[i].Amount > fm.Events[j].Amount })

		// hints: the few habit categories where money actually moves
		for i, c := range cats {
			if i >= 3 {
				break
			}
			if c.ForecastIDR >= 50000 {
				fm.Hints = append(fm.Hints, fmt.Sprintf(
					"%s: rata-rata %s/bulan dari %d bulan — %s",
					c.Category, rupiahText(c.ForecastIDR), c.MonthsSeen,
					polaHint(c.Pattern)))
			}
		}
		out = append(out, fm)
	}

	// what is already planned for the target month: commitments, not guesses
	type plan struct {
		ID       int64  `json:"id"`
		Kind     string `json:"kind"`
		Label    string `json:"label"`
		Amount   int64  `json:"amount_idr"`
		Variable bool   `json:"variable"`
		Day      int    `json:"day_of_month"`
	}
	plans := []plan{}
	prows, err := db.Query(scheduleSQL, userID, month+"-01", false)
	if err == nil {
		listed, err2 := scanSchedules(prows)
		prows.Close()
		if err2 == nil {
			for _, s := range fillEstimates(listed) {
				if s.Status == "not_started" || s.Status == "finished" {
					continue
				}
				amt := int64(0)
				label := ""
				switch s.Kind {
				case "expense":
					label = "bayar " + s.Payee
					if s.AmountIDR != nil {
						amt = *s.AmountIDR
					} else if s.EstimateIDR != nil {
						amt = *s.EstimateIDR
					}
				case "income":
					label = "masuk → " + s.To
					if s.AmountIDR != nil {
						amt = *s.AmountIDR
					}
				default:
					label = s.From + " → " + s.To
					if s.AmountIDR != nil {
						amt = *s.AmountIDR
					}
					if strings.Contains(strings.ToLower(s.To), "tabungan") {
						label += " (tabungan)"
					}
				}
				plans = append(plans, plan{ID: s.ID, Kind: s.Kind, Label: label, Amount: amt,
					Variable: s.AmountIDR == nil, Day: s.DayOfMonth})
			}
		}
	}

	writeJSON(w, 200, map[string]any{
		"month":        month,
		"based_on":     history,
		"window":       window,
		"members":      out,
		"plans":        plans,
		"generated_at": time.Now().Format(time.RFC3339),
	})
}

func polaHint(pattern string) string {
	switch pattern {
	case "rutin":
		return "muncul tiap bulan, ini yang paling bisa ditekan"
	case "jarang":
		return "jarang muncul, tapi tetap dihitung (dibagi rata)"
	default:
		return "pola tetap"
	}
}

func max1(n int64) int64 {
	if n < 1 {
		return 1
	}
	return n
}

func rupiahText(n int64) string {
	s := fmt.Sprintf("%d", n)
	out := ""
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out += "."
		}
		out += string(c)
	}
	return "Rp" + out
}
