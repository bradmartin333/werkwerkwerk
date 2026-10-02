package main

import (
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

//go:embed templates/*.html
var tmplFS embed.FS

const (
	minReps     = 5
	maxReps     = 200
	sessionName = "werk"
	sessionTTL  = 365 * 24 * time.Hour
	dayFmt      = "2006-01-02"
)

var (
	db    *sql.DB
	pages = template.Must(template.New("").Funcs(template.FuncMap{
		"pct": func(f float64) string { return fmt.Sprintf("%+.1f%%", f) },
		"inc": func(i int) int { return i + 1 },
	}).ParseFS(tmplFS, "templates/*.html"))
	repOptions = func() []int {
		r := make([]int, 0, maxReps-minReps+1)
		for i := minReps; i <= maxReps; i++ {
			r = append(r, i)
		}
		return r
	}()
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id      INTEGER PRIMARY KEY,
	name    TEXT NOT NULL UNIQUE,
	pw_hash TEXT NOT NULL,
	w1 TEXT, w2 TEXT, w3 TEXT
);
CREATE TABLE IF NOT EXISTS entries (
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	day     TEXT NOT NULL,
	r1 INTEGER NOT NULL, r2 INTEGER NOT NULL, r3 INTEGER NOT NULL,
	PRIMARY KEY (user_id, day)
);
CREATE TABLE IF NOT EXISTS sessions (
	token   TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	expires INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`

func main() {
	path := os.Getenv("WERK_DB")
	if path == "" {
		path = "werk.db"
	}
	var err error
	db, err = sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		log.Fatal(err)
	}
	if _, err := db.Exec(schema); err != nil {
		log.Fatal(err)
	}

	args := os.Args[1:]
	if len(args) == 0 || args[0] == "serve" {
		serve()
		return
	}
	if err := runCLI(args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// ---------- CLI ----------

const usage = `werk commands:
  werk serve                    run the web server (default)
  werk users                    list users
  werk adduser <name> <pass>    add a user
  werk passwd  <name> <pass>    reset a password
  werk deluser <name>           delete a user and ALL their data
  werk enddate <YYYY-MM-DD>     set the last day of the challenge
  werk enddate none             clear the end date
  werk standings                print everyone's % change
  werk plot [name] > out.png    PNG of everyone's progress, or one user's reps
  werk reset --yes              back up the db, then wipe entries, workouts and
                                end date for a new challenge (keeps users)`

func runCLI(args []string) error {
	need := func(n int) error {
		if len(args) != n {
			return errors.New("wrong number of args\n\n" + usage)
		}
		return nil
	}
	switch args[0] {
	case "users":
		rows, err := db.Query(`SELECT u.name, COALESCE(u.w1,'(not set up)'), COALESCE(u.w2,''), COALESCE(u.w3,''),
			(SELECT COUNT(*) FROM entries e WHERE e.user_id = u.id) FROM users u ORDER BY u.name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name, w1, w2, w3 string
			var n int
			rows.Scan(&name, &w1, &w2, &w3, &n)
			fmt.Printf("%-12s %3d days  %s | %s | %s\n", name, n, w1, w2, w3)
		}
		if end := endDate(); end != "" {
			fmt.Println("end date:", end)
		}
		return rows.Err()
	case "adduser", "passwd":
		if err := need(3); err != nil {
			return err
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(args[2]), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		name := normName(args[1])
		if args[0] == "adduser" {
			_, err = db.Exec(`INSERT INTO users (name, pw_hash) VALUES (?, ?)`, name, hash)
		} else {
			err = mustAffect(db.Exec(`UPDATE users SET pw_hash = ? WHERE name = ?`, hash, name))
			if err == nil {
				db.Exec(`DELETE FROM sessions WHERE user_id = (SELECT id FROM users WHERE name = ?)`, name)
			}
		}
		if err == nil {
			fmt.Println("ok:", name)
		}
		return err
	case "deluser":
		if err := need(2); err != nil {
			return err
		}
		if err := mustAffect(db.Exec(`DELETE FROM users WHERE name = ?`, normName(args[1]))); err != nil {
			return err
		}
		fmt.Println("deleted:", normName(args[1]))
		return nil
	case "enddate":
		if err := need(2); err != nil {
			return err
		}
		if args[1] == "none" {
			_, err := db.Exec(`DELETE FROM settings WHERE key = 'end_date'`)
			return err
		}
		if _, err := time.Parse(dayFmt, args[1]); err != nil {
			return errors.New("date must look like 2026-12-31")
		}
		_, err := db.Exec(`INSERT INTO settings (key, value) VALUES ('end_date', ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, args[1])
		if err == nil {
			fmt.Println("end date:", args[1])
		}
		return err
	case "standings":
		for i, s := range standings() {
			fmt.Printf("%2d. %-12s %8s  %3d days\n", i+1, s.Name, fmt.Sprintf("%+.1f%%", s.Pct), s.Days)
		}
		return nil
	case "plot":
		if len(args) > 2 {
			return errors.New("wrong number of args\n\n" + usage)
		}
		if fi, _ := os.Stdout.Stat(); fi != nil && fi.Mode()&os.ModeCharDevice != 0 {
			return errors.New("redirect the PNG to a file: docker exec werk /werk plot > progress.png (no -t)")
		}
		if len(args) == 2 {
			return plotUser(os.Stdout, normName(args[1]))
		}
		return plotAll(os.Stdout)
	case "reset":
		if len(args) != 2 || args[1] != "--yes" {
			return errors.New("this wipes every entry, workout and the end date (users and passwords stay).\nrun `werk reset --yes` to do it")
		}
		return reset()
	}
	return errors.New(usage)
}

// reset snapshots the db next to itself, then clears everything but the
// accounts so everyone picks new workouts on their next visit.
func reset() error {
	path := os.Getenv("WERK_DB")
	if path == "" {
		path = "werk.db"
	}
	backup := strings.TrimSuffix(path, ".db") + "-" + time.Now().Format("20060102-150405") + ".db"
	if _, err := db.Exec(`VACUUM INTO ?`, backup); err != nil {
		return fmt.Errorf("backup failed, nothing reset: %w", err)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM entries`,
		`UPDATE users SET w1 = NULL, w2 = NULL, w3 = NULL`,
		`DELETE FROM settings WHERE key = 'end_date'`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Println("backed up to", backup)
	fmt.Println("reset: entries, workouts and end date cleared. set a new one with `werk enddate`.")
	return nil
}

func mustAffect(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("no such user")
	}
	return nil
}

func normName(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// ---------- data ----------

type user struct {
	ID       int64
	Name     string
	Workouts [3]string
}

func (u *user) setUp() bool { return u.Workouts[0] != "" }

func today() string { return time.Now().Format(dayFmt) }

func endDate() string {
	var v string
	db.QueryRow(`SELECT value FROM settings WHERE key = 'end_date'`).Scan(&v)
	return v
}

// daysLeft returns days remaining after today (0 = today is the last day),
// whether an end date is set, and whether the challenge is over.
func daysLeft() (left int, set, over bool) {
	end := endDate()
	if end == "" {
		return 0, false, false
	}
	e, _ := time.ParseInLocation(dayFmt, end, time.Local)
	t, _ := time.ParseInLocation(dayFmt, today(), time.Local)
	left = int(math.Round(e.Sub(t).Hours() / 24))
	return left, true, left < 0
}

// progress is the mean, across the three workouts, of the % change from the
// first logged day to the most recent one. Averaging percentages keeps a
// 150-rep workout from drowning out a 10-rep one.
func progress(uid int64) (pct float64, days int) {
	var first, last [3]int
	q := `SELECT r1, r2, r3 FROM entries WHERE user_id = ? ORDER BY day `
	if db.QueryRow(q+"ASC LIMIT 1", uid).Scan(&first[0], &first[1], &first[2]) != nil {
		return 0, 0
	}
	db.QueryRow(q+"DESC LIMIT 1", uid).Scan(&last[0], &last[1], &last[2])
	db.QueryRow(`SELECT COUNT(*) FROM entries WHERE user_id = ?`, uid).Scan(&days)
	for i := range first {
		pct += float64(last[i]-first[i]) / float64(first[i]) * 100 // first[i] >= minReps, never 0
	}
	return pct / 3, days
}

type standing struct {
	Name string
	Pct  float64
	Days int
}

// standings lists everyone with at least one entry, best progress first.
func standings() []standing {
	rows, err := db.Query(`SELECT id, name FROM users ORDER BY name`)
	if err != nil {
		return nil
	}
	type idName struct {
		id   int64
		name string
	}
	var us []idName
	for rows.Next() {
		var u idName
		rows.Scan(&u.id, &u.name)
		us = append(us, u)
	}
	rows.Close()
	var out []standing
	for _, u := range us {
		if pct, days := progress(u.id); days > 0 {
			out = append(out, standing{u.name, pct, days})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Pct > out[j].Pct })
	return out
}

// ---------- web ----------

func serve() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) { render(w, "login", nil) })
	mux.HandleFunc("POST /login", login)
	mux.HandleFunc("POST /logout", logout)
	mux.HandleFunc("GET /{$}", auth(home))
	mux.HandleFunc("POST /setup", auth(setup))
	mux.HandleFunc("POST /entry", auth(entry))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	addr := os.Getenv("WERK_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	log.Printf("werk listening on %s (tz %s)", addr, time.Local)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := pages.ExecuteTemplate(w, name, data); err != nil {
		log.Println("render:", err)
	}
}

func auth(next func(http.ResponseWriter, *http.Request, *user)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionName)
		if err == nil {
			u := &user{}
			var w1, w2, w3 sql.NullString
			err = db.QueryRow(`SELECT u.id, u.name, u.w1, u.w2, u.w3 FROM sessions s JOIN users u ON u.id = s.user_id
				WHERE s.token = ? AND s.expires > ?`, c.Value, time.Now().Unix()).Scan(&u.ID, &u.Name, &w1, &w2, &w3)
			if err == nil {
				u.Workouts = [3]string{w1.String, w2.String, w3.String}
				next(w, r, u)
				return
			}
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

func login(w http.ResponseWriter, r *http.Request) {
	var id int64
	var hash string
	err := db.QueryRow(`SELECT id, pw_hash FROM users WHERE name = ?`, normName(r.FormValue("name"))).Scan(&id, &hash)
	if err == nil {
		err = bcrypt.CompareHashAndPassword([]byte(hash), []byte(r.FormValue("password")))
	}
	if err != nil {
		time.Sleep(time.Second)
		render(w, "login", "NOPE. TRY AGAIN.")
		return
	}
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	db.Exec(`DELETE FROM sessions WHERE expires < ?`, time.Now().Unix())
	db.Exec(`INSERT INTO sessions (token, user_id, expires) VALUES (?, ?, ?)`, token, id, time.Now().Add(sessionTTL).Unix())
	http.SetCookie(w, &http.Cookie{
		Name: sessionName, Value: token, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: r.Header.Get("X-Forwarded-Proto") == "https" || r.TLS != nil, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionName); err == nil {
		db.Exec(`DELETE FROM sessions WHERE token = ?`, c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionName, Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type row struct {
	Name  string
	Value int
}

type homeData struct {
	User     *user
	Rows     []row
	Options  []int
	Logged   bool
	EndSet   bool
	DaysLeft int
	Pct      float64
	Days     int
	Error    string
	Standing []standing
}

func home(w http.ResponseWriter, r *http.Request, u *user) {
	if !u.setUp() {
		render(w, "setup", homeData{User: u})
		return
	}
	left, set, over := daysLeft()
	if over {
		pct, days := progress(u.ID)
		render(w, "final", homeData{User: u, Pct: pct, Days: days, Standing: standings()})
		return
	}
	// Prefill with today's entry, else the last one, so the wheel starts near
	// where they'll land.
	v := [3]int{minReps, minReps, minReps}
	var day string
	db.QueryRow(`SELECT day, r1, r2, r3 FROM entries WHERE user_id = ? ORDER BY day DESC LIMIT 1`, u.ID).
		Scan(&day, &v[0], &v[1], &v[2])
	rows := make([]row, 3)
	for i := range rows {
		rows[i] = row{u.Workouts[i], v[i]}
	}
	render(w, "entry", homeData{User: u, Rows: rows, Options: repOptions, Logged: day == today(), EndSet: set, DaysLeft: left})
}

func setup(w http.ResponseWriter, r *http.Request, u *user) {
	var ws [3]string
	for i := range ws {
		ws[i] = strings.TrimSpace(r.FormValue(fmt.Sprintf("w%d", i+1)))
		if ws[i] == "" || len(ws[i]) > 40 {
			render(w, "setup", homeData{User: u, Error: "NEED 3 WORKOUTS (40 CHARS MAX)"})
			return
		}
	}
	// "w1 IS NULL" makes workouts write-once even if the form is replayed.
	db.Exec(`UPDATE users SET w1 = ?, w2 = ?, w3 = ? WHERE id = ? AND w1 IS NULL`, ws[0], ws[1], ws[2], u.ID)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func entry(w http.ResponseWriter, r *http.Request, u *user) {
	if _, _, over := daysLeft(); over || !u.setUp() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	var v [3]int
	for i := range v {
		n, err := strconv.Atoi(r.FormValue(fmt.Sprintf("r%d", i+1)))
		if err != nil || n < minReps || n > maxReps {
			http.Error(w, "bad reps", http.StatusBadRequest)
			return
		}
		v[i] = n
	}
	_, err := db.Exec(`INSERT INTO entries (user_id, day, r1, r2, r3) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(user_id, day) DO UPDATE SET r1 = excluded.r1, r2 = excluded.r2, r3 = excluded.r3`,
		u.ID, today(), v[0], v[1], v[2])
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	// Rendered straight from the POST (no redirect); the page rewrites its URL
	// to "/" so a reload lands back on the entry screen.
	pct, days := progress(u.ID)
	left, set, _ := daysLeft()
	render(w, "result", homeData{User: u, Pct: pct, Days: days, EndSet: set, DaysLeft: left})
}
