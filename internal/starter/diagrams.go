package starter

import (
	"encoding/base64"
	"strings"
)

// The tutorial's diagrams. Kept as readable SVG source rather than as
// base64 blobs so they can be reviewed and edited like any other code, and
// encoded into data URLs at build time (internal/imagedata explains why the
// picture travels inside the widget rather than in the blob store).
//
// Diagrams, not screenshots, on purpose: a screenshot of the console inside
// the console goes stale on the next UI change — the developer manual's
// captures already do — while a picture of how the pieces fit together
// stays true as long as the pieces do.

const (
	ink    = "#0f172a"
	muted  = "#64748b"
	accent = "#4f46e5"
	line   = "#cbd5e1"
	fill   = "#eef2ff"
	paper  = "#ffffff"
)

func dataURL(svg string) string {
	return "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(strings.TrimSpace(svg)))
}

// svgHead opens an SVG that scales to its widget and reads as a diagram.
func svgHead(w, h int) string {
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ` + itoa(w) + ` ` + itoa(h) + `" font-family="ui-sans-serif,-apple-system,Segoe UI,Roboto,sans-serif">`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// box draws a labelled rounded rectangle with an optional second line.
func box(x, y, w, h int, label, sub string, strong bool) string {
	stroke, bg, weight := line, paper, "600"
	if strong {
		stroke, bg = accent, fill
	}
	s := `<rect x="` + itoa(x) + `" y="` + itoa(y) + `" width="` + itoa(w) + `" height="` + itoa(h) +
		`" rx="10" fill="` + bg + `" stroke="` + stroke + `" stroke-width="1.5"/>`
	ty := y + h/2
	if sub != "" {
		ty = y + h/2 - 7
	}
	s += `<text x="` + itoa(x+w/2) + `" y="` + itoa(ty) + `" text-anchor="middle" dominant-baseline="middle" font-size="15" font-weight="` + weight + `" fill="` + ink + `">` + label + `</text>`
	if sub != "" {
		s += `<text x="` + itoa(x+w/2) + `" y="` + itoa(y+h/2+12) + `" text-anchor="middle" dominant-baseline="middle" font-size="12" fill="` + muted + `">` + sub + `</text>`
	}
	return s
}

// arrow points right from (x,y) for length px.
func arrow(x, y, length int) string {
	return `<line x1="` + itoa(x) + `" y1="` + itoa(y) + `" x2="` + itoa(x+length-8) + `" y2="` + itoa(y) +
		`" stroke="` + muted + `" stroke-width="1.5"/><path d="M` + itoa(x+length) + ` ` + itoa(y) + `l-9-4v8z" fill="` + muted + `"/>`
}

// arrowDown points down from (x,y) for length px.
func arrowDown(x, y, length int) string {
	return `<line x1="` + itoa(x) + `" y1="` + itoa(y) + `" x2="` + itoa(x) + `" y2="` + itoa(y+length-8) +
		`" stroke="` + muted + `" stroke-width="1.5"/><path d="M` + itoa(x) + ` ` + itoa(y+length) + `l-4-9h8z" fill="` + muted + `"/>`
}

// caption writes a line of small muted text; anchor is start, middle or end.
func caption(x, y int, anchor, s string) string {
	return `<text x="` + itoa(x) + `" y="` + itoa(y) + `" text-anchor="` + anchor + `" font-size="12" fill="` + muted + `">` + s + `</text>`
}

// buildFlow: what a model is made of, and what you make from it. You lay
// out the grid and the dashboard; the platform works out the numbers in them.
func buildFlow() string {
	s := svgHead(860, 150)
	s += box(10, 40, 170, 70, "Dimensions", "what you slice by", false)
	s += caption(95, 28, "middle", "team, quarter")
	s += box(200, 40, 170, 70, "Metrics", "what you measure", false)
	s += caption(285, 28, "middle", "headcount, cost")
	s += arrow(378, 75, 34)
	s += box(420, 40, 170, 70, "Grid", "where numbers live", true)
	s += arrow(598, 75, 34)
	s += box(640, 40, 210, 70, "Dashboard", "what people look at", true)
	s += caption(190, 132, "middle", "you define these")
	s += caption(635, 132, "middle", "you lay these out; the platform works out totals and calculations")
	return s + `</svg>`
}

// thousands writes n with a comma between each group of three digits, the
// way the grid shows a number.
func thousands(n int) string {
	s := itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

// oneNumberWidth is the width of the oneNumber picture and of its widget.
const oneNumberWidth = 640

// oneNumber: how a single value is addressed. Its labels and the highlighted
// value come from the example's own data, so the picture shows the number
// the grid holds: cost for the first team in the first quarter. It returns
// the picture's height too: the height follows the table, and the widget
// that shows it must be the same height or the picture gains empty bands.
func oneNumber() (svg string, height int) {
	const mid = oneNumberWidth / 2
	var s string
	cols := make([]string, len(quarters))
	for i, q := range quarters {
		cols[i] = q.code
	}
	rows := make([]string, len(teams))
	for i, t := range teams {
		rows[i] = t.label
	}
	value := thousands(int(teams[0].headcount[0] * teams[0].perHead))
	x0, y0, cw, ch := 150, 36, 90, 44
	for i, c := range cols {
		s += `<text x="` + itoa(x0+i*cw+cw/2) + `" y="` + itoa(y0-12) + `" text-anchor="middle" font-size="13" font-weight="600" fill="` + ink + `">` + c + `</text>`
	}
	for r, name := range rows {
		s += `<text x="` + itoa(x0-12) + `" y="` + itoa(y0+r*ch+ch/2) + `" text-anchor="end" dominant-baseline="middle" font-size="13" font-weight="600" fill="` + ink + `">` + name + `</text>`
		for c := range cols {
			x, y := x0+c*cw, y0+r*ch
			bg, stroke := paper, line
			if r == 0 && c == 0 {
				bg, stroke = fill, accent
			}
			s += `<rect x="` + itoa(x) + `" y="` + itoa(y) + `" width="` + itoa(cw) + `" height="` + itoa(ch) + `" fill="` + bg + `" stroke="` + stroke + `" stroke-width="1.5"/>`
		}
	}
	s += `<text x="` + itoa(x0+cw/2) + `" y="` + itoa(y0+ch/2) + `" text-anchor="middle" dominant-baseline="middle" font-size="14" font-weight="700" fill="` + accent + `">` + value + `</text>`
	// A dashed lead from the cell to the words that address it, below the table.
	lead := y0 + len(rows)*ch + 18
	s += `<line x1="` + itoa(x0+cw/2) + `" y1="` + itoa(y0+ch+6) + `" x2="` + itoa(x0+cw/2) + `" y2="` + itoa(lead) + `" stroke="` + accent + `" stroke-width="1.5" stroke-dasharray="4 3"/>`
	s += `<line x1="` + itoa(x0+cw/2) + `" y1="` + itoa(lead) + `" x2="` + itoa(mid) + `" y2="` + itoa(lead) + `" stroke="` + accent + `" stroke-width="1.5" stroke-dasharray="4 3"/>`
	s += `<text x="` + itoa(mid) + `" y="` + itoa(lead+24) + `" text-anchor="middle" font-size="13" fill="` + ink + `">team = <tspan font-weight="700">` + rows[0] + `</tspan> · quarter = <tspan font-weight="700">` + cols[0] + `</tspan> · metric = <tspan font-weight="700">cost</tspan></text>`
	s += `<text x="` + itoa(mid) + `" y="` + itoa(lead+46) + `" text-anchor="middle" font-size="12" fill="` + muted + `">one member of each dimension, one metric, one number</text>`
	height = lead + 64
	return svgHead(oneNumberWidth, height) + s + `</svg>`, height
}

// revisions: the unit of change. One revision is live; New revision makes a
// copy to work in, and Set active makes that copy the live one.
func revisions() string {
	s := svgHead(760, 190)
	s += box(20, 65, 150, 60, "Model", "Learn the platform", false)
	// A bracket from the model to each of its revisions.
	s += `<path d="M170 95h22M192 46v98" fill="none" stroke="` + muted + `" stroke-width="1.5"/>`
	s += arrow(192, 46, 26)
	s += arrow(192, 144, 26)
	s += `<rect x="220" y="20" width="240" height="52" rx="10" fill="` + fill + `" stroke="` + accent + `" stroke-width="1.5"/>`
	s += `<text x="240" y="46" dominant-baseline="middle" font-size="15" font-weight="600" fill="` + ink + `">First revision</text>`
	s += `<rect x="392" y="35" width="48" height="22" rx="11" fill="#dcfce7" stroke="#16a34a"/><text x="416" y="47" text-anchor="middle" dominant-baseline="middle" font-size="11" font-weight="700" fill="#15803d">live</text>`
	s += arrowDown(300, 76, 38)
	s += `<text x="310" y="99" dominant-baseline="middle" font-size="12" font-weight="600" fill="` + accent + `">New revision</text>`
	s += box(220, 118, 240, 52, "Next revision", "a copy you can change", false)
	s += caption(478, 42, "start", "what everyone sees")
	s += caption(478, 58, "start", "a change here shows at once")
	s += caption(478, 140, "start", "out of everyone's way;")
	s += caption(478, 156, "start", "Set active makes it the live one")
	return s + `</svg>`
}

// roles: who does what, and which roles the sign-up account holds.
func roles() string {
	s := svgHead(780, 206)
	type role struct {
		name, what string
		yours      bool
	}
	rs := []role{
		{"Business user", "enters numbers, fills in forms, starts requests", false},
		{"Business admin", "acts on requests; sets who sees which pages and data", true},
		{"Developer", "builds models: metrics, grids, dashboards, forms, workflows", true},
		{"Tenant admin", "invites people, grants roles, runs the workspace", true},
	}
	for i, r := range rs {
		y := 12 + i*48
		stroke, bg := line, paper
		if r.yours {
			stroke, bg = accent, fill
		}
		s += `<rect x="12" y="` + itoa(y) + `" width="200" height="38" rx="8" fill="` + bg + `" stroke="` + stroke + `" stroke-width="1.5"/>`
		s += `<text x="112" y="` + itoa(y+19) + `" text-anchor="middle" dominant-baseline="middle" font-size="14" font-weight="600" fill="` + ink + `">` + r.name + `</text>`
		s += `<text x="232" y="` + itoa(y+19) + `" dominant-baseline="middle" font-size="13" fill="` + muted + `">` + r.what + `</text>`
		if r.yours {
			s += `<rect x="664" y="` + itoa(y+8) + `" width="104" height="22" rx="11" fill="` + paper + `" stroke="` + accent + `"/>`
			s += `<text x="716" y="` + itoa(y+19) + `" text-anchor="middle" dominant-baseline="middle" font-size="11" font-weight="700" fill="` + accent + `">your account</text>`
		}
	}
	return s + `</svg>`
}
