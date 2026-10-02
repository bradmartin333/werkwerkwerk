package main

import (
	"bytes"
	"image/png"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newDB points the global db at a fresh file in a temp dir.
func newDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "werk.db")
	if err := openDB(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	t.Setenv("WERK_DB", path)
	return path
}

func mustCLI(t *testing.T, args ...string) {
	t.Helper()
	if err := runCLI(args); err != nil {
		t.Fatalf("werk %s: %v", strings.Join(args, " "), err)
	}
}

// addPlayer creates a set-up user with an entry per day, ending yesterday.
func addPlayer(t *testing.T, name string, reps ...[3]int) int64 {
	t.Helper()
	mustCLI(t, "adduser", name, "pw")
	var id int64
	db.QueryRow(`SELECT id FROM users WHERE name = ?`, name).Scan(&id)
	db.Exec(`UPDATE users SET w1 = 'a', w2 = 'b', w3 = 'c' WHERE id = ?`, id)
	for i, r := range reps {
		day := time.Now().AddDate(0, 0, i-len(reps)).Format(dayFmt)
		if _, err := db.Exec(`INSERT INTO entries VALUES (?, ?, ?, ?, ?)`, id, day, r[0], r[1], r[2]); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestScore(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries [][3]int
		want    float64
	}{
		{"empty", nil, 0},
		{"one day", [][3]int{{10, 20, 30}}, 0},
		{"doubled everything", [][3]int{{10, 20, 30}, {20, 40, 60}}, 50}, // avg(10,20)=15 → +50%
		{"one workout up", [][3]int{{10, 10, 10}, {10, 10, 10}, {10, 10, 10}, {40, 10, 10}, {40, 10, 10}, {40, 10, 10}}, 100},
		// Last 3 are 30, 30, 15: avg 25 vs 10 → +150%. A last-day-only score would say +50%.
		{"off day is smoothed", [][3]int{{10, 10, 10}, {30, 30, 30}, {30, 30, 30}, {15, 15, 15}}, 150},
		{"went down", [][3]int{{20, 20, 20}, {10, 10, 10}, {10, 10, 10}, {10, 10, 10}}, -50},
	} {
		if got := score(tc.entries); !near(got, tc.want) {
			t.Errorf("%s: score = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestProgressAndStandings(t *testing.T) {
	newDB(t)
	a := addPlayer(t, "fred", [3]int{10, 10, 10}, [3]int{20, 20, 20})
	addPlayer(t, "jeff", [3]int{10, 10, 10}, [3]int{40, 40, 40})
	addPlayer(t, "bob")
	mustCLI(t, "adduser", "newbie", "pw")

	if pct, days := progress(a); !near(pct, 50) || days != 2 {
		t.Errorf("fred progress = %v, %d days; want 50, 2", pct, days)
	}
	s := standings()
	if len(s) != 2 || s[0].Name != "jeff" || s[1].Name != "fred" {
		t.Fatalf("standings = %+v, want jeff then fred (no zero-entry users)", s)
	}
	if !near(s[0].Pct, 150) {
		t.Errorf("jeff = %v, want 150", s[0].Pct)
	}
}

func TestDaysLeft(t *testing.T) {
	newDB(t)
	if _, set, _ := daysLeft(); set {
		t.Fatal("no end date should mean not set")
	}
	for _, tc := range []struct {
		offset int
		left   int
		over   bool
	}{{5, 5, false}, {0, 0, false}, {-1, -1, true}} {
		mustCLI(t, "enddate", time.Now().AddDate(0, 0, tc.offset).Format(dayFmt))
		left, set, over := daysLeft()
		if !set || left != tc.left || over != tc.over {
			t.Errorf("end today%+d: got left=%d over=%v, want %d %v", tc.offset, left, over, tc.left, tc.over)
		}
	}
	mustCLI(t, "enddate", "none")
	if _, set, _ := daysLeft(); set {
		t.Error("enddate none should clear it")
	}
	if runCLI([]string{"enddate", "12/31/2026"}) == nil {
		t.Error("bad date format should fail")
	}
}

func TestReset(t *testing.T) {
	path := newDB(t)
	addPlayer(t, "fred", [3]int{10, 10, 10}, [3]int{20, 20, 20})
	mustCLI(t, "enddate", "2026-12-31")

	if runCLI([]string{"reset"}) == nil {
		t.Fatal("reset without --yes should refuse")
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM entries`).Scan(&n)
	if n != 2 {
		t.Fatalf("refused reset still deleted entries (%d left)", n)
	}

	mustCLI(t, "reset", "--yes")
	db.QueryRow(`SELECT COUNT(*) FROM entries`).Scan(&n)
	if n != 0 {
		t.Errorf("%d entries survived reset", n)
	}
	var w1 *string
	var hash string
	if err := db.QueryRow(`SELECT w1, pw_hash FROM users WHERE name = 'fred'`).Scan(&w1, &hash); err != nil {
		t.Fatal("user should survive reset:", err)
	}
	if w1 != nil {
		t.Error("workouts should be cleared")
	}
	if endDate() != "" {
		t.Error("end date should be cleared")
	}

	backups, _ := filepath.Glob(strings.TrimSuffix(path, ".db") + "-*.db")
	if len(backups) != 1 {
		t.Fatalf("want 1 backup, found %v", backups)
	}
	if err := openDB(backups[0]); err != nil {
		t.Fatal(err)
	}
	db.QueryRow(`SELECT COUNT(*) FROM entries`).Scan(&n)
	if n != 2 {
		t.Errorf("backup has %d entries, want 2", n)
	}
}

func TestPlots(t *testing.T) {
	newDB(t)
	if plotAll(io.Discard) == nil {
		t.Error("plotting with no data should error")
	}
	addPlayer(t, "fred", [3]int{10, 20, 30}, [3]int{12, 22, 35}, [3]int{15, 25, 40})
	addPlayer(t, "jeff", [3]int{50, 50, 50})

	for _, name := range []string{"", "fred", "jeff"} {
		var buf bytes.Buffer
		var err error
		if name == "" {
			err = plotAll(&buf)
		} else {
			err = plotUser(&buf, name)
		}
		if err != nil {
			t.Fatalf("plot %q: %v", name, err)
		}
		img, err := png.Decode(&buf)
		if err != nil {
			t.Fatalf("plot %q: not a PNG: %v", name, err)
		}
		if b := img.Bounds(); b.Dx() != plotW*plotScale || b.Dy() != plotH*plotScale {
			t.Errorf("plot %q is %v", name, b)
		}
	}
	if plotUser(io.Discard, "nobody") == nil {
		t.Error("unknown user should error")
	}
}

func TestNiceTicks(t *testing.T) {
	for _, tc := range [][2]float64{{0, 77}, {-3, 73}, {0, 0}, {5, 500}, {12.5, 13}} {
		ticks := niceTicks(tc[0], tc[1], 6)
		if ticks[0] > tc[0] || ticks[len(ticks)-1] < tc[1] {
			t.Errorf("niceTicks%v = %v doesn't cover the range", tc, ticks)
		}
		if len(ticks) < 2 || len(ticks) > 12 {
			t.Errorf("niceTicks%v = %v: odd tick count", tc, ticks)
		}
	}
}

// TestWebFlow drives the real handlers: log in, set up, log reps, game over.
func TestWebFlow(t *testing.T) {
	newDB(t)
	mustCLI(t, "adduser", "fred", "pw")
	addPlayer(t, "jeff", [3]int{10, 10, 10}, [3]int{30, 30, 30})
	srv := httptest.NewServer(routes())
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

	page := func(resp *http.Response, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	post := func(path string, form url.Values) (*http.Response, error) { return c.PostForm(srv.URL+path, form) }

	if body := page(c.Get(srv.URL + "/")); !strings.Contains(body, "Log in") {
		t.Fatal("logged-out visitor should get the login page")
	}
	start := time.Now()
	if body := page(post("/login", url.Values{"name": {"fred"}, "password": {"nope"}})); !strings.Contains(body, "NOPE") {
		t.Error("wrong password should be rejected")
	}
	if time.Since(start) < time.Second {
		t.Error("failed login should be slowed down")
	}
	if body := page(post("/login", url.Values{"name": {" Fred "}, "password": {"pw"}})); !strings.Contains(body, "Pick your 3 workouts") {
		t.Fatal("new player should land on setup")
	}
	page(post("/setup", url.Values{"w1": {"push-ups"}, "w2": {"squats"}, "w3": {"sit-ups"}}))
	body := page(c.Get(srv.URL + "/"))
	if !strings.Contains(body, "push-ups") || !strings.Contains(body, "<option>500</option>") {
		t.Fatal("entry page should list workouts and reps up to 500")
	}

	resp, _ := post("/entry", url.Values{"r1": {"501"}, "r2": {"10"}, "r3": {"10"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("501 reps: status %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	if body := page(post("/entry", url.Values{"r1": {"500"}, "r2": {"10"}, "r3": {"10"}})); !strings.Contains(body, "&#43;0.0%") {
		t.Error("first entry should score +0.0%")
	}

	mustCLI(t, "enddate", time.Now().AddDate(0, 0, -1).Format(dayFmt))
	body = page(c.Get(srv.URL + "/"))
	if !strings.Contains(body, "GAME OVER") || !strings.Contains(body, "STANDINGS") {
		t.Fatal("after the end date everyone should see game over + standings")
	}
	if strings.Index(body, "jeff") > strings.Index(body, "fred") || !strings.Contains(body, "&#43;100.0%") {
		t.Error("standings should rank jeff (+100%) above fred")
	}
	resp, _ = post("/entry", url.Values{"r1": {"20"}, "r2": {"20"}, "r3": {"20"}})
	resp.Body.Close()
	var r1 int
	db.QueryRow(`SELECT r1 FROM entries e JOIN users u ON u.id = e.user_id WHERE u.name = 'fred'`).Scan(&r1)
	if r1 != 500 {
		t.Error("entries after game over should be ignored")
	}

	font, err := c.Get(srv.URL + "/static/VT323-Regular.ttf")
	if err != nil || font.StatusCode != 200 || !strings.Contains(font.Header.Get("Cache-Control"), "immutable") {
		t.Error("embedded font should be served with a long cache")
	}
	font.Body.Close()
}
