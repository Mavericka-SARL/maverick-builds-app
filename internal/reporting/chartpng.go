package reporting

// A chart as a PNG image: what a host without in-chat UI (MCP Apps) shows
// in the conversation instead of the interactive chart. Drawn here from the
// same ChartSpec, so the picture and the table beside it agree.

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const (
	pngW, pngH            = 960, 540
	plotL, plotR          = 84, 24
	plotT, plotB          = 72, 96
	legendSwatch, legendH = 12, 18
)

var (
	palette = []color.RGBA{
		{37, 99, 235, 255}, {245, 158, 11, 255}, {16, 185, 129, 255}, {239, 68, 68, 255}, {139, 92, 246, 255},
	}
	ink      = color.RGBA{31, 41, 55, 255}
	muted    = color.RGBA{107, 114, 128, 255}
	gridline = color.RGBA{229, 231, 235, 255}

	faceOnce          sync.Once
	titleFace, textFc font.Face
)

func faces() (font.Face, font.Face) {
	faceOnce.Do(func() {
		f, err := opentype.Parse(goregular.TTF)
		if err != nil {
			panic(err)
		}
		titleFace, _ = opentype.NewFace(f, &opentype.FaceOptions{Size: 18, DPI: 72, Hinting: font.HintingFull})
		textFc, _ = opentype.NewFace(f, &opentype.FaceOptions{Size: 12, DPI: 72, Hinting: font.HintingFull})
	})
	return titleFace, textFc
}

type canvas struct {
	img        *image.RGBA
	title, txt font.Face
}

func (c *canvas) rect(x0, y0, x1, y1 int, col color.Color) {
	if x1 < x0 {
		x0, x1 = x1, x0
	}
	if y1 < y0 {
		y0, y1 = y1, y0
	}
	draw.Draw(c.img, image.Rect(x0, y0, x1, y1), &image.Uniform{col}, image.Point{}, draw.Src)
}

func (c *canvas) line(x0, y0, x1, y1 int, col color.Color, width int) {
	dx, dy := math.Abs(float64(x1-x0)), math.Abs(float64(y1-y0))
	steps := int(math.Max(dx, dy))
	if steps == 0 {
		steps = 1
	}
	for i := 0; i <= steps; i++ {
		x := x0 + (x1-x0)*i/steps
		y := y0 + (y1-y0)*i/steps
		c.rect(x-width/2, y-width/2, x-width/2+width, y-width/2+width, col)
	}
}

func (c *canvas) dot(x, y, r int, col color.Color) {
	for dy := -r; dy <= r; dy++ {
		for dx := -r; dx <= r; dx++ {
			if dx*dx+dy*dy <= r*r {
				c.img.Set(x+dx, y+dy, col)
			}
		}
	}
}

func (c *canvas) text(x, y int, s string, face font.Face, col color.Color) {
	d := &font.Drawer{Dst: c.img, Src: &image.Uniform{col}, Face: face, Dot: fixed.P(x, y)}
	d.DrawString(s)
}

func (c *canvas) width(s string, face font.Face) int {
	return (&font.Drawer{Face: face}).MeasureString(s).Ceil()
}

// fit shortens s to at most px wide.
func (c *canvas) fit(s string, px int) string {
	if c.width(s, c.txt) <= px {
		return s
	}
	r := []rune(s)
	for len(r) > 1 && c.width(string(r)+"…", c.txt) > px {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

// RenderPNG draws spec.
func RenderPNG(spec *ChartSpec) ([]byte, error) {
	tf, xf := faces()
	c := &canvas{img: image.NewRGBA(image.Rect(0, 0, pngW, pngH)), title: tf, txt: xf}
	c.rect(0, 0, pngW, pngH, color.White)
	c.text(plotL, 30, c.fit(spec.Title, pngW-plotL-plotR), tf, ink)
	if spec.Subtitle != "" {
		c.text(plotL, 50, c.fit(spec.Subtitle, pngW-plotL-plotR), xf, muted)
	}
	switch spec.Kind {
	case KindPie:
		c.pie(spec)
	case KindScatter:
		c.scatter(spec)
	case KindHistogram:
		c.histogram(spec)
	default:
		c.categorical(spec)
	}
	if len(spec.Notes) > 0 {
		c.text(plotL, pngH-12, c.fit(strings.Join(spec.Notes, " · "), pngW-plotL-plotR), xf, muted)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, c.img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// axis is a value axis from lo to hi with round ticks.
type axis struct{ lo, hi, step float64 }

func niceAxis(lo, hi float64, includeZero bool) axis {
	if includeZero {
		lo, hi = math.Min(lo, 0), math.Max(hi, 0)
	}
	if hi == lo {
		hi = lo + 1
	}
	raw := (hi - lo) / 5
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	step := mag
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if mag*m >= raw {
			step = mag * m
			break
		}
	}
	return axis{lo: math.Floor(lo/step) * step, hi: math.Ceil(hi/step) * step, step: step}
}

func (a axis) pos(v float64, from, to int) int {
	return from + int(float64(to-from)*(v-a.lo)/(a.hi-a.lo))
}

func compact(v float64) string {
	av := math.Abs(v)
	switch {
	case av >= 1e9:
		return trimZero(fmt.Sprintf("%.1f", v/1e9)) + "B"
	case av >= 1e6:
		return trimZero(fmt.Sprintf("%.1f", v/1e6)) + "M"
	case av >= 1e3:
		return trimZero(fmt.Sprintf("%.1f", v/1e3)) + "K"
	case av == math.Trunc(av):
		return fmt.Sprintf("%.0f", v)
	}
	return trimZero(fmt.Sprintf("%.2f", v))
}

func trimZero(s string) string {
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// yAxis draws horizontal grid lines and tick labels.
func (c *canvas) yAxis(a axis, top, bottom int, format func(float64) string) {
	for v := a.lo; v <= a.hi+a.step/2; v += a.step {
		y := a.pos(v, bottom, top)
		c.line(plotL, y, pngW-plotR, y, gridline, 1)
		lbl := format(v)
		c.text(plotL-8-c.width(lbl, c.txt), y+4, lbl, c.txt, muted)
	}
}

func (c *canvas) legend(names []string) {
	x := pngW - plotR
	for i := len(names) - 1; i >= 0; i-- {
		w := c.width(names[i], c.txt) + legendSwatch + 16
		x -= w
		c.rect(x, 58, x+legendSwatch, 58+legendSwatch, palette[i%len(palette)])
		c.text(x+legendSwatch+5, 69, names[i], c.txt, ink)
	}
}

func (c *canvas) categorical(spec *ChartSpec) {
	top, bottom := plotT+legendH, pngH-plotB
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, s := range spec.Series {
		for _, v := range s.Values {
			if v != nil {
				lo, hi = math.Min(lo, *v), math.Max(hi, *v)
			}
		}
	}
	if math.IsInf(lo, 0) {
		lo, hi = 0, 1
	}
	a := niceAxis(lo, hi, spec.Kind == KindBar)
	c.yAxis(a, top, bottom, compact)
	var names []string
	for _, s := range spec.Series {
		names = append(names, s.Label)
	}
	if len(names) > 1 {
		c.legend(names)
	}
	n := len(spec.Categories)
	if n == 0 {
		return
	}
	slot := float64(pngW-plotL-plotR) / float64(n)
	every := int(math.Max(1, math.Ceil(float64(n)*60/float64(pngW-plotL-plotR))))
	for i, cat := range spec.Categories {
		if i%every == 0 {
			lbl := c.fit(cat.Name, int(slot*float64(every))-4)
			cx := plotL + int(slot*(float64(i)+0.5))
			c.text(cx-c.width(lbl, c.txt)/2, bottom+18, lbl, c.txt, ink)
		}
	}
	zero := a.pos(math.Max(a.lo, math.Min(0, a.hi)), bottom, top)
	if spec.Kind == KindLine {
		for si, s := range spec.Series {
			col := palette[si%len(palette)]
			px, py, has := 0, 0, false
			for i, v := range s.Values {
				if v == nil {
					has = false // a gap, not a zero
					continue
				}
				x, y := plotL+int(slot*(float64(i)+0.5)), a.pos(*v, bottom, top)
				if has {
					c.line(px, py, x, y, col, 2)
				}
				c.dot(x, y, 3, col)
				px, py, has = x, y, true
			}
		}
		return
	}
	group := slot * 0.8
	bw := group / float64(len(spec.Series))
	for si, s := range spec.Series {
		col := palette[si%len(palette)]
		for i, v := range s.Values {
			if v == nil {
				continue
			}
			x0 := plotL + int(slot*float64(i)+slot*0.1+bw*float64(si))
			c.rect(x0, zero, x0+int(math.Max(1, bw-2)), a.pos(*v, bottom, top), col)
		}
	}
	c.line(plotL, zero, pngW-plotR, zero, muted, 1)
}

func (c *canvas) pie(spec *ChartSpec) {
	s := spec.Series[0]
	total := 0.0
	for _, v := range s.Values {
		if v != nil {
			total += *v
		}
	}
	if total <= 0 {
		return
	}
	cx, cy, r := 300, (plotT+pngH-plotB)/2+10, 170
	start := -math.Pi / 2
	type wedge struct{ a0, a1 float64 }
	wedges := make([]wedge, len(s.Values))
	for i, v := range s.Values {
		share := *v / total
		wedges[i] = wedge{start, start + share*2*math.Pi}
		start = wedges[i].a1
	}
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			dx, dy := float64(x-cx), float64(y-cy)
			if dx*dx+dy*dy > float64(r*r) {
				continue
			}
			ang := math.Atan2(dy, dx)
			if ang < -math.Pi/2 {
				ang += 2 * math.Pi
			}
			for i, w := range wedges {
				if ang >= w.a0 && ang < w.a1 {
					c.img.Set(x, y, pieColor(i))
					break
				}
			}
		}
	}
	for i, cat := range spec.Categories {
		if i >= 16 {
			c.text(560, 110+i*22, fmt.Sprintf("… %d more", len(spec.Categories)-16), c.txt, muted)
			break
		}
		y := 100 + i*22
		c.rect(560, y, 560+legendSwatch, y+legendSwatch, pieColor(i))
		c.text(580, y+11, c.fit(fmt.Sprintf("%s — %s (%.1f%%)", cat.Name, compact(*s.Values[i]), *s.Values[i]/total*100), 360), c.txt, ink)
	}
}

func pieColor(i int) color.RGBA {
	base := palette[i%len(palette)]
	if round := i / len(palette); round > 0 {
		f := 1 - 0.25*float64(round%3)
		return color.RGBA{uint8(float64(base.R) * f), uint8(float64(base.G) * f), uint8(float64(base.B) * f), 255}
	}
	return base
}

func (c *canvas) scatter(spec *ChartSpec) {
	top, bottom := plotT+legendH, pngH-plotB
	if len(spec.Points) == 0 {
		return
	}
	xlo, xhi, ylo, yhi := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
	for _, p := range spec.Points {
		xlo, xhi, ylo, yhi = math.Min(xlo, p.X), math.Max(xhi, p.X), math.Min(ylo, p.Y), math.Max(yhi, p.Y)
	}
	ax, ay := niceAxis(xlo, xhi, false), niceAxis(ylo, yhi, false)
	c.yAxis(ay, top, bottom, compact)
	for v := ax.lo; v <= ax.hi+ax.step/2; v += ax.step {
		x := ax.pos(v, plotL, pngW-plotR)
		lbl := compact(v)
		c.text(x-c.width(lbl, c.txt)/2, bottom+18, lbl, c.txt, muted)
	}
	for _, p := range spec.Points {
		c.dot(ax.pos(p.X, plotL, pngW-plotR), ay.pos(p.Y, bottom, top), 4, palette[0])
	}
	if spec.XAxis != nil && spec.YAxis != nil {
		c.text(pngW/2-60, bottom+42, "x: "+spec.XAxis.Label, c.txt, ink)
		c.text(plotL, top-6, "y: "+spec.YAxis.Label, c.txt, ink)
	}
}

func (c *canvas) histogram(spec *ChartSpec) {
	top, bottom := plotT+legendH, pngH-plotB
	maxCount := 1
	for _, b := range spec.Bins {
		maxCount = max(maxCount, b.Count)
	}
	a := niceAxis(0, float64(maxCount), true)
	c.yAxis(a, top, bottom, func(v float64) string { return fmt.Sprintf("%.0f", v) })
	n := len(spec.Bins)
	if n == 0 {
		return
	}
	slot := float64(pngW-plotL-plotR) / float64(n)
	for i, b := range spec.Bins {
		x0 := plotL + int(slot*float64(i))
		c.rect(x0+1, bottom, x0+int(slot)-1, a.pos(float64(b.Count), bottom, top), palette[0])
		lbl := compact(b.Min)
		c.text(x0-c.width(lbl, c.txt)/2, bottom+18, lbl, c.txt, muted)
	}
	last := compact(spec.Bins[n-1].Max)
	c.text(pngW-plotR-c.width(last, c.txt)/2, bottom+18, last, c.txt, muted)
	c.text(plotL, top-6, fmt.Sprintf("members per range (population %d)", spec.Population), c.txt, ink)
}
