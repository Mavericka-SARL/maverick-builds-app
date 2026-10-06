package importpkg

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

// A workbook stores a date as a number (days since 1899-12-30) and only its
// number format makes it a date. Read as stored values, a Hire Date column
// arrived as "41967": a text metric kept the serial and a pick-list or
// dimension code never matched. isoDates writes each date-formatted cell as
// ISO text instead ("2014-11-24", with "T15:04:05" when it has a time), and
// a number column reads that text back as the same serial (dateSerial), so
// a numeric metric still receives what it always did.
func isoDates(f *excelize.File, sheet string, grid [][]string) {
	props, _ := f.GetWorkbookProps()
	use1904 := props.Date1904 != nil && *props.Date1904
	dateStyle := map[int]bool{}
	for r, row := range grid {
		for c, raw := range row {
			v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
			if err != nil || v < 0 {
				continue
			}
			ref, err := excelize.CoordinatesToCellName(c+1, r+1)
			if err != nil {
				continue
			}
			id, err := f.GetCellStyle(sheet, ref)
			if err != nil || id == 0 {
				continue
			}
			isDate, seen := dateStyle[id]
			if !seen {
				if st, err := f.GetStyle(id); err == nil {
					custom := ""
					if st.CustomNumFmt != nil {
						custom = *st.CustomNumFmt
					}
					isDate = isDateFormat(st.NumFmt, custom)
				}
				dateStyle[id] = isDate
			}
			if !isDate {
				continue
			}
			t, err := excelize.ExcelDateToTime(v, use1904)
			if err != nil {
				continue
			}
			if t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 {
				grid[r][c] = t.Format("2006-01-02")
			} else {
				grid[r][c] = t.Format("2006-01-02T15:04:05")
			}
		}
	}
}

// isDateFormat reports whether a number format shows a calendar date: a
// built-in date format, or a custom one naming a year or a day outside its
// quoted text, [colour/locale] sections and escaped characters. Time-only
// formats (h:mm) are not dates: their cells keep their numbers.
func isDateFormat(numFmt int, custom string) bool {
	switch {
	case numFmt >= 14 && numFmt <= 17, numFmt == 22, numFmt >= 27 && numFmt <= 36, numFmt >= 50 && numFmt <= 58:
		return true
	case custom == "":
		return false
	}
	inQuote, inBracket, escaped := false, false, false
	for _, ch := range strings.ToLower(custom) {
		switch {
		case escaped:
			escaped = false
		case ch == '\\' || ch == '_' || ch == '*':
			escaped = true // the next character is literal (or a width/fill)
		case ch == '"':
			inQuote = !inQuote
		case inQuote:
		case ch == '[':
			inBracket = true
		case ch == ']':
			inBracket = false
		case inBracket:
		case ch == 'y' || ch == 'd':
			return true
		}
	}
	return false
}

// excelEpoch is day 0 of the 1900 date system, as the formula engine counts.
var excelEpoch = time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)

// dateSerial reads ISO date text — what isoDates writes for a date cell —
// as the workbook's serial number: ok false for anything else.
func dateSerial(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 10 || s[4] != '-' || s[7] != '-' {
		return 0, false
	}
	for _, layout := range []string{"2006-01-02", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			days := t.Sub(excelEpoch).Hours() / 24
			return math.Round(days*1e9) / 1e9, true
		}
	}
	return 0, false
}

// storedNumbers puts back a plain number cell's stored value into a grid
// read as displayed: "1,234.50" for a #,##0.00 value was refused by the
// number check, so a finance workbook's upload failed on every value row.
// A date cell keeps its displayed text (a member code may be written that
// way), and so does a percentage, whose scale the engine has not settled
// (0.25 reads 0.25% in a grid and 25% on a KPI). raw is the same sheet read
// as stored values.
func storedNumbers(f *excelize.File, sheet string, displayed, raw [][]string) {
	keep := map[int]bool{} // style id → keeps its displayed text
	for r, row := range displayed {
		if r >= len(raw) {
			return
		}
		for c, shown := range row {
			if c >= len(raw[r]) || raw[r][c] == shown {
				continue
			}
			if _, err := strconv.ParseFloat(strings.TrimSpace(raw[r][c]), 64); err != nil {
				continue
			}
			ref, err := excelize.CoordinatesToCellName(c+1, r+1)
			if err != nil {
				continue
			}
			id, err := f.GetCellStyle(sheet, ref)
			if err != nil {
				continue
			}
			k, seen := keep[id]
			if !seen {
				if st, err := f.GetStyle(id); err == nil {
					custom := ""
					if st.CustomNumFmt != nil {
						custom = *st.CustomNumFmt
					}
					k = isDateFormat(st.NumFmt, custom) || isPercentFormat(st.NumFmt, custom)
				}
				keep[id] = k
			}
			if !k {
				displayed[r][c] = raw[r][c]
			}
		}
	}
}

// isPercentFormat reports whether a number format shows a percentage: the
// built-in 0% and 0.00%, or a custom one with % outside quoted text.
func isPercentFormat(numFmt int, custom string) bool {
	if numFmt == 9 || numFmt == 10 {
		return true
	}
	inQuote, escaped := false, false
	for _, ch := range custom {
		switch {
		case escaped:
			escaped = false
		case ch == '\\':
			escaped = true
		case ch == '"':
			inQuote = !inQuote
		case !inQuote && ch == '%':
			return true
		}
	}
	return false
}
