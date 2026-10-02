package main

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// Charts are drawn at 2x so they stay crisp on phones and retina screens.
const (
	plotW, plotH = 1400, 820
	plotScale    = 2
)

var (
	inkPrimary   = hexColor("#000000")
	inkSecondary = hexColor("#52514e")
	paper        = hexColor("#fffdf5")
	grid         = hexColor("#e4e2d8")
	axis         = hexColor("#8f8d84")
	// Validated categorical order (adjacent CVD ΔE ≥ 9 on #fffdf5). Hues are
	// assigned in this order and never cycled; past 8 series, extras go gray.
	seriesColors = []color.RGBA{
		hexColor("#2a78d6"), hexColor("#eb6834"), hexColor("#1baf7a"), hexColor("#eda100"),
		hexColor("#e87ba4"), hexColor("#008300"), hexColor("#4a3aa7"), hexColor("#e34948"),
	}
)

type point struct {
	day time.Time
	v   float64
}

type series struct {
	label string // shown in the legend
	end   string // value printed at the line's end
	pts   []point
}

// plotAll charts every user's progress score (same formula as the app) as of
// each day they logged.
func plotAll(w io.Writer) error {
	rows, err := db.Query(`SELECT u.name, e.day, e.r1, e.r2, e.r3 FROM entries e
		JOIN users u ON u.id = e.user_id ORDER BY u.name, e.day`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var all []series
	var es [][3]int
	for rows.Next() {
		var name, day string
		var r [3]int
		if err := rows.Scan(&name, &day, &r[0], &r[1], &r[2]); err != nil {
			return err
		}
		if len(all) == 0 || all[len(all)-1].label != name {
			all = append(all, series{label: name})
			es = nil
		}
		es = append(es, r)
		d, _ := time.Parse(dayFmt, day)
		s := &all[len(all)-1]
		s.pts = append(s.pts, point{d, score(es)})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(all) == 0 {
		return errors.New("nothing logged yet")
	}
	for i := range all {
		all[i].end = fmt.Sprintf("%+.1f%%", all[i].pts[len(all[i].pts)-1].v)
	}
	return drawChart(w, "WERK // PROGRESS", "% change from day 1 to the average of the last 3 days logged", "%", true, all)
}

// plotUser charts one user's raw reps for each of their three workouts.
func plotUser(w io.Writer, name string) error {
	var uid int64
	var ws [3]string
	err := db.QueryRow(`SELECT id, COALESCE(w1,''), COALESCE(w2,''), COALESCE(w3,'') FROM users WHERE name = ?`, name).
		Scan(&uid, &ws[0], &ws[1], &ws[2])
	if err != nil {
		return errors.New("no such user")
	}
	rows, err := db.Query(`SELECT day, r1, r2, r3 FROM entries WHERE user_id = ? ORDER BY day`, uid)
	if err != nil {
		return err
	}
	defer rows.Close()
	all := make([]series, 3)
	for i := range all {
		all[i].label = ws[i]
	}
	for rows.Next() {
		var day string
		var r [3]int
		if err := rows.Scan(&day, &r[0], &r[1], &r[2]); err != nil {
			return err
		}
		d, _ := time.Parse(dayFmt, day)
		for i := range all {
			all[i].pts = append(all[i].pts, point{d, float64(r[i])})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(all[0].pts) == 0 {
		return errors.New(name + " hasn't logged anything yet")
	}
	for i := range all {
		all[i].end = fmt.Sprint(all[i].pts[len(all[i].pts)-1].v)
	}
	pct, days := progress(uid)
	sub := fmt.Sprintf("reps per day  /  %d day%s logged  /  %+.1f%% since day 1", days, plural(days), pct)
	return drawChart(w, "WERK // "+strings.ToUpper(name), sub, "", false, all)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func drawChart(w io.Writer, title, subtitle, unit string, zeroLine bool, all []series) error {
	c := newCanvas(plotW, plotH)
	titleFace, err := face(gomonobold.TTF, 34)
	if err != nil {
		return err
	}
	textFace, _ := face(gomono.TTF, 20)
	labelFace, _ := face(gomonobold.TTF, 20)

	// Window chrome to match the app: black frame with a hard drop shadow.
	c.rect(14, 14, plotW-4, plotH-4, inkPrimary)
	c.rect(4, 4, plotW-14, plotH-14, inkPrimary)
	c.rect(8, 8, plotW-18, plotH-18, paper)

	c.text(titleFace, 48, 70, title, inkPrimary)
	c.text(textFace, 48, 104, subtitle, inkSecondary)

	// Legend: swatch + name, always present for 2+ series.
	lx := 48.0
	if len(all) > 1 {
		for i, s := range all {
			c.rect(lx, 128, lx+22, 140, colorFor(i))
			c.text(textFace, lx+30, 141, s.label, inkPrimary)
			lx += 30 + c.measure(textFace, s.label) + 30
		}
	}

	// Data extents.
	minD, maxD := all[0].pts[0].day, all[0].pts[0].day
	minV, maxV := math.Inf(1), math.Inf(-1)
	for _, s := range all {
		for _, p := range s.pts {
			if p.day.Before(minD) {
				minD = p.day
			}
			if p.day.After(maxD) {
				maxD = p.day
			}
			minV, maxV = math.Min(minV, p.v), math.Max(maxV, p.v)
		}
	}
	if zeroLine {
		minV, maxV = math.Min(minV, 0), math.Max(maxV, 0)
	} else {
		minV = 0 // reps: anchor at zero so growth isn't exaggerated
	}
	if maxD.Equal(minD) {
		minD, maxD = minD.AddDate(0, 0, -1), maxD.AddDate(0, 0, 1)
	}
	ticks := niceTicks(minV, maxV, 6)
	minV, maxV = ticks[0], ticks[len(ticks)-1]

	// End labels need room on the right; measure the widest.
	labelW := 0.0
	for _, s := range all {
		labelW = math.Max(labelW, c.measure(labelFace, s.end)+c.measure(textFace, " "+s.label))
	}
	left, top, bottom := 110.0, 180.0, float64(plotH)-80
	right := float64(plotW) - 60 - labelW - 28
	x := func(d time.Time) float64 {
		return left + d.Sub(minD).Hours()/maxD.Sub(minD).Hours()*(right-left)
	}
	y := func(v float64) float64 { return bottom - (v-minV)/(maxV-minV)*(bottom-top) }

	// Gridlines + y ticks.
	for _, t := range ticks {
		col := grid
		if zeroLine && t == 0 {
			col = axis
		}
		c.rect(left, y(t)-1, right, y(t)+1, col)
		lbl := fmt.Sprintf("%g%s", t, unit)
		if zeroLine && t > 0 {
			lbl = "+" + lbl
		}
		c.text(textFace, left-14-c.measure(textFace, lbl), y(t)+7, lbl, inkSecondary)
	}
	c.rect(left, bottom-1, right, bottom+1, axis)

	// X ticks: about six evenly spaced days.
	span := int(math.Round(maxD.Sub(minD).Hours() / 24))
	step := max(1, int(math.Ceil(float64(span)/6)))
	for d := 0; d <= span; d += step {
		day := minD.AddDate(0, 0, d)
		lbl := day.Format("Jan 2")
		c.rect(x(day)-1, bottom, x(day)+1, bottom+8, axis)
		c.text(textFace, x(day)-c.measure(textFace, lbl)/2, bottom+34, lbl, inkSecondary)
	}

	// Lines: 3px with round joins, then end dots with a surface ring.
	type endLabel struct {
		y, dotX, dotY float64
		s             series
	}
	var ends []endLabel
	for i, s := range all {
		col := colorFor(i)
		var path [][2]float64
		for _, p := range s.pts {
			path = append(path, [2]float64{x(p.day), y(p.v)})
		}
		c.polyline(path, 3, col)
		last := path[len(path)-1]
		c.disc(last[0], last[1], 9, paper)
		c.disc(last[0], last[1], 6.5, col)
		ends = append(ends, endLabel{last[1], last[0], last[1], s})
	}

	// Direct end labels. When two collide, spread them and draw a thin
	// leader back to the dot instead of floating free.
	sort.Slice(ends, func(i, j int) bool { return ends[i].y < ends[j].y })
	const gap = 30.0
	for i := 1; i < len(ends); i++ {
		ends[i].y = math.Max(ends[i].y, ends[i-1].y+gap)
	}
	if over := ends[len(ends)-1].y - (bottom + 6); over > 0 {
		for i := range ends {
			ends[i].y -= over
		}
	}
	lx = right + 28
	for _, e := range ends {
		if math.Abs(e.y-e.dotY) > 2 || e.dotX < right-1 {
			c.polyline([][2]float64{{e.dotX + 10, e.dotY}, {lx - 6, e.y}}, 1.5, axis)
		}
		c.text(labelFace, lx, e.y+7, e.s.end, inkPrimary)
		c.text(textFace, lx+c.measure(labelFace, e.s.end), e.y+7, " "+e.s.label, inkSecondary)
	}

	return png.Encode(w, c.img)
}

func colorFor(i int) color.RGBA {
	if i < len(seriesColors) {
		return seriesColors[i]
	}
	return hexColor("#9a988f")
}

// niceTicks returns round tick values covering [lo, hi].
func niceTicks(lo, hi float64, n int) []float64 {
	if hi-lo < 1e-9 {
		lo, hi = lo-1, hi+1
	}
	raw := (hi - lo) / float64(n-1)
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	step := mag * 10
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if raw <= m*mag {
			step = m * mag
			break
		}
	}
	var out []float64
	for t := math.Floor(lo/step) * step; t <= hi+step*1e-9 || len(out) < 2; t += step {
		out = append(out, math.Round(t*1e6)/1e6)
	}
	if out[len(out)-1] < hi {
		out = append(out, out[len(out)-1]+step)
	}
	return out
}

// ---------- tiny anti-aliased canvas ----------

type canvas struct{ img *image.RGBA }

func newCanvas(w, h int) *canvas {
	img := image.NewRGBA(image.Rect(0, 0, w*plotScale, h*plotScale))
	draw.Draw(img, img.Bounds(), image.NewUniform(paper), image.Point{}, draw.Src)
	return &canvas{img}
}

func (c *canvas) rect(x0, y0, x1, y1 float64, col color.RGBA) {
	c.fill([][2]float64{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}}, col)
}

// fill rasterizes polygons in logical coords. Each is wound the same way so
// overlapping shapes add up instead of cancelling.
func (c *canvas) fill(poly [][2]float64, col color.RGBA, more ...[][2]float64) {
	b := c.img.Bounds()
	r := vector.NewRasterizer(b.Dx(), b.Dy())
	for _, p := range append([][][2]float64{poly}, more...) {
		area := 0.0
		for i := range p {
			j := (i + 1) % len(p)
			area += p[i][0]*p[j][1] - p[j][0]*p[i][1]
		}
		if area < 0 {
			for i, j := 0, len(p)-1; i < j; i, j = i+1, j-1 {
				p[i], p[j] = p[j], p[i]
			}
		}
		r.MoveTo(float32(p[0][0]*plotScale), float32(p[0][1]*plotScale))
		for _, q := range p[1:] {
			r.LineTo(float32(q[0]*plotScale), float32(q[1]*plotScale))
		}
		r.ClosePath()
	}
	r.Draw(c.img, b, image.NewUniform(col), image.Point{})
}

func circle(cx, cy, rad float64) [][2]float64 {
	const n = 32
	p := make([][2]float64, n)
	for i := range p {
		a := float64(i) / n * 2 * math.Pi
		p[i] = [2]float64{cx + rad*math.Cos(a), cy + rad*math.Sin(a)}
	}
	return p
}

func (c *canvas) disc(cx, cy, rad float64, col color.RGBA) { c.fill(circle(cx, cy, rad), col) }

// polyline strokes a path: one quad per segment plus a disc at every vertex
// for round joins and caps, all in a single fill so nothing double-blends.
func (c *canvas) polyline(pts [][2]float64, width float64, col color.RGBA) {
	h := width / 2
	var polys [][][2]float64
	for i, p := range pts {
		polys = append(polys, circle(p[0], p[1], h))
		if i == 0 {
			continue
		}
		q := pts[i-1]
		dx, dy := p[0]-q[0], p[1]-q[1]
		l := math.Hypot(dx, dy)
		if l == 0 {
			continue
		}
		nx, ny := -dy/l*h, dx/l*h
		polys = append(polys, [][2]float64{{q[0] + nx, q[1] + ny}, {p[0] + nx, p[1] + ny}, {p[0] - nx, p[1] - ny}, {q[0] - nx, q[1] - ny}})
	}
	c.fill(polys[0], col, polys[1:]...)
}

func face(ttf []byte, size float64) (font.Face, error) {
	f, err := opentype.Parse(ttf)
	if err != nil {
		return nil, err
	}
	return opentype.NewFace(f, &opentype.FaceOptions{Size: size * plotScale, DPI: 72, Hinting: font.HintingFull})
}

func (c *canvas) text(f font.Face, x, y float64, s string, col color.RGBA) {
	d := font.Drawer{Dst: c.img, Src: image.NewUniform(col), Face: f,
		Dot: fixed.P(int(x*plotScale), int(y*plotScale))}
	d.DrawString(s)
}

func (c *canvas) measure(f font.Face, s string) float64 {
	return float64(font.MeasureString(f, s).Round()) / plotScale
}

func hexColor(s string) color.RGBA {
	var r, g, b uint8
	fmt.Sscanf(s, "#%02x%02x%02x", &r, &g, &b)
	return color.RGBA{r, g, b, 255}
}
