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

// buildFlow: what a model is made of, and what it becomes.
func buildFlow() string {
	s := svgHead(860, 150)
	s += box(10, 40, 170, 70, "Dimensions", "what you slice by", false)
	s += box(10, 40, 170, 70, "Dimensions", "what you slice by", false)
	s += `<text x="95" y="28" text-anchor="middle" font-size="12" fill="` + muted + `">team, quarter</text>`
	s += box(200, 40, 170, 70, "Metrics", "what you measure", false)
	s += `<text x="285" y="28" text-anchor="middle" font-size="12" fill="` + muted + `">headcount, cost</text>`
	s += arrow(378, 75, 34)
	s += box(420, 40, 170, 70, "Grid", "where numbers live", true)
	s += arrow(598, 75, 34)
	s += box(640, 40, 210, 70, "Dashboard", "what people look at", true)
	s += `<text x="190" y="132" text-anchor="middle" font-size="12" fill="` + muted + `">you define these</text>`
	s += `<text x="640" y="132" text-anchor="middle" font-size="12" fill="` + muted + `">the platform builds these from them</text>`
	return s + `</svg>`
}

// oneNumber: how a single value is addressed.
func oneNumber() string {
	const mid = 320
	s := svgHead(640, 230)
	cols := []string{"Q1", "Q2", "Q3", "Q4"}
	rows := []string{"Sales", "Engineering"}
	x0, y0, cw, ch := 150, 60, 90, 44
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
	s += `<text x="` + itoa(x0+cw/2) + `" y="` + itoa(y0+ch/2) + `" text-anchor="middle" dominant-baseline="middle" font-size="14" font-weight="700" fill="` + accent + `">48,000</text>`
	s += `<line x1="` + itoa(x0+cw/2) + `" y1="` + itoa(y0+ch+6) + `" x2="` + itoa(x0+cw/2) + `" y2="166" stroke="` + accent + `" stroke-width="1.5" stroke-dasharray="4 3"/>`
	s += `<line x1="` + itoa(x0+cw/2) + `" y1="166" x2="` + itoa(mid) + `" y2="166" stroke="` + accent + `" stroke-width="1.5" stroke-dasharray="4 3"/>`
	s += `<text x="` + itoa(mid) + `" y="190" text-anchor="middle" font-size="13" fill="` + ink + `">team = <tspan font-weight="700">Sales</tspan> · quarter = <tspan font-weight="700">Q1</tspan> · metric = <tspan font-weight="700">cost</tspan></text>`
	s += `<text x="` + itoa(mid) + `" y="212" text-anchor="middle" font-size="12" fill="` + muted + `">one member of each dimension, one metric, one number</text>`
	return s + `</svg>`
}

// revisions: the unit of change.
func revisions() string {
	s := svgHead(620, 170)
	s += box(20, 55, 150, 60, "Model", "Learn the platform", false)
	s += arrow(178, 85, 32)
	s += `<rect x="220" y="20" width="230" height="52" rx="10" fill="` + fill + `" stroke="` + accent + `" stroke-width="1.5"/>`
	s += `<text x="240" y="46" dominant-baseline="middle" font-size="15" font-weight="600" fill="` + ink + `">First revision</text>`
	s += `<rect x="382" y="35" width="48" height="22" rx="11" fill="#dcfce7" stroke="#16a34a"/><text x="406" y="47" text-anchor="middle" dominant-baseline="middle" font-size="11" font-weight="700" fill="#15803d">live</text>`
	s += box(220, 96, 230, 52, "Next revision", "a copy you can change", false)
	s += `<text x="466" y="50" font-size="12" fill="` + muted + `">what everyone sees</text>`
	s += `<text x="466" y="126" font-size="12" fill="` + muted + `">safe to rework</text>`
	return s + `</svg>`
}

// roles: who does what.
func roles() string {
	s := svgHead(760, 210)
	type role struct{ name, what string }
	rs := []role{
		{"Business user", "enters numbers, reads dashboards"},
		{"Business admin", "the above, plus who may see what"},
		{"Developer", "builds the model: dimensions, metrics, grids"},
		{"Tenant admin", "people, access and the workspace itself"},
	}
	for i, r := range rs {
		y := 12 + i*48
		s += `<rect x="12" y="` + itoa(y) + `" width="200" height="38" rx="8" fill="` + fill + `" stroke="` + accent + `" stroke-width="1.5"/>`
		s += `<text x="112" y="` + itoa(y+19) + `" text-anchor="middle" dominant-baseline="middle" font-size="14" font-weight="600" fill="` + ink + `">` + r.name + `</text>`
		s += `<text x="232" y="` + itoa(y+19) + `" dominant-baseline="middle" font-size="13" fill="` + muted + `">` + r.what + `</text>`
	}
	return s + `</svg>`
}
