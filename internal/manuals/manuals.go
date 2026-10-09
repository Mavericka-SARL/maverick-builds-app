// Package manuals searches the Developer and Formulas manuals the console
// ships, section by section, for the AI Developer's read_manual tool: the
// assistant explains how the platform works from what its documentation
// says rather than from what it guesses.
package manuals

import (
	"fmt"
	"html"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/mavericks-engine/mavericks/docs"
)

// Section is one h2 or h3 section of a manual, as plain text.
type Section struct {
	Manual  string // "developer" | "formulas"
	Chapter string // its h2
	Title   string // its own heading (the chapter's, for the text before its first h3)
	Text    string
}

var (
	once     sync.Once
	sections []Section

	headingRE = regexp.MustCompile(`(?is)<h([23])[^>]*>(.*?)</h[23]>`)
	dropRE    = regexp.MustCompile(`(?is)<(style|script|head)[^>]*>.*?</(style|script|head)>`)
	breakRE   = regexp.MustCompile(`(?i)<(br|/p|/li|/tr|/h[1-6]|/div|/pre|/table)[^>]*>`)
	cellRE    = regexp.MustCompile(`(?i)</t[dh]>`)
	tagRE     = regexp.MustCompile(`(?s)<[^>]+>`)
	spaceRE   = regexp.MustCompile(`[ \t]+`)
	blankRE   = regexp.MustCompile(`\n\s*\n+`)
)

func load() {
	once.Do(func() {
		sections = append(split("developer", docs.DeveloperManual), split("formulas", docs.FormulasManual)...)
	})
}

// split cuts a manual into its h2/h3 sections.
func split(manual, doc string) []Section {
	doc = dropRE.ReplaceAllString(doc, "")
	idx := headingRE.FindAllStringSubmatchIndex(doc, -1)
	var out []Section
	chapter := ""
	for i, m := range idx {
		level := doc[m[2]:m[3]]
		title := plain(doc[m[4]:m[5]])
		end := len(doc)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		if level == "2" {
			chapter = title
		}
		text := plain(doc[m[1]:end])
		if strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, Section{Manual: manual, Chapter: chapter, Title: title, Text: text})
	}
	return out
}

// plain is HTML as readable text: line breaks kept, tags and entities gone.
func plain(s string) string {
	s = breakRE.ReplaceAllString(s, "\n")
	s = cellRE.ReplaceAllString(s, " | ")
	s = tagRE.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = spaceRE.ReplaceAllString(s, " ")
	s = blankRE.ReplaceAllString(s, "\n")
	return strings.TrimSpace(s)
}

// Contents lists every section of manual ("" = both) by chapter.
func Contents(manual string) string {
	load()
	var b strings.Builder
	last := ""
	for _, s := range sections {
		if manual != "" && s.Manual != manual {
			continue
		}
		head := s.Manual + " manual — " + s.Chapter
		if head != last {
			fmt.Fprintf(&b, "\n%s\n", head)
			last = head
		}
		if s.Title != s.Chapter {
			fmt.Fprintf(&b, "  · %s\n", s.Title)
		}
	}
	return strings.TrimSpace(b.String())
}

// Search returns the sections of manual ("" = both) that best match query,
// most relevant first, their text cut so the answer stays under limit
// characters.
func Search(query, manual string, limit int) []Section {
	load()
	terms := words(query)
	if len(terms) == 0 {
		return nil
	}
	type hit struct {
		s     Section
		score float64
	}
	var hits []hit
	for _, s := range sections {
		if manual != "" && s.Manual != manual {
			continue
		}
		title := strings.ToLower(s.Chapter + " " + s.Title)
		body := strings.ToLower(s.Text)
		score := 0.0
		matched := 0
		for _, t := range terms {
			n := strings.Count(body, t)
			if strings.Contains(title, t) {
				score += 5
			}
			if n > 0 || strings.Contains(title, t) {
				matched++
			}
			score += float64(min(n, 8))
		}
		if matched == 0 {
			continue
		}
		// Every term found counts most: a section about all of a question
		// beats one that repeats a single word.
		score *= float64(matched) / float64(len(terms))
		hits = append(hits, hit{s, score})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	var out []Section
	used := 0
	for _, h := range hits {
		if len(out) == 4 || used >= limit {
			break
		}
		s := h.s
		if room := limit - used; len(s.Text) > room {
			s.Text = s.Text[:max(room, 400)] + " …"
		}
		used += len(s.Text)
		out = append(out, s)
	}
	return out
}

// stop words carry no topic.
var stop = map[string]bool{"the": true, "a": true, "an": true, "and": true, "or": true, "of": true, "to": true, "in": true,
	"is": true, "how": true, "do": true, "i": true, "what": true, "for": true, "on": true, "with": true, "can": true, "does": true, "it": true}

func words(q string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_'
	}) {
		if len(w) < 2 || stop[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}
