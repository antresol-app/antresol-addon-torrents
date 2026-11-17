package cardigann

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Dates are passed between filters as RFC1123Z strings, like Prowlarr does
// with its Rfc1123ZPattern. Russian words are normalised to English first so
// «Вчера 10:00», «5 мар 24», «сегодня в 12:30», «5 минут назад» all parse.

const dateLayout = time.RFC1123Z

func formatDate(t time.Time) string { return t.Format(dateLayout) }

// ruWord maps a lower-case Russian word (by prefix) to English.
var ruWords = []struct{ prefix, en string }{
	{"сегодня", "today"}, {"вчера", "yesterday"}, {"завтра", "tomorrow"},
	{"сейчас", "now"}, {"только", "now"}, {"назад", "ago"},
	{"янв", "Jan"}, {"фев", "Feb"}, {"мар", "Mar"}, {"апр", "Apr"}, {"мая", "May"}, {"май", "May"},
	{"июн", "Jun"}, {"июл", "Jul"}, {"авг", "Aug"}, {"сен", "Sep"}, {"окт", "Oct"}, {"ноя", "Nov"}, {"дек", "Dec"},
	{"сек", "sec"}, {"мин", "min"}, {"час", "hour"}, {"дн", "day"}, {"ден", "day"}, {"сут", "day"},
	{"нед", "week"}, {"мес", "month"}, {"год", "year"}, {"лет", "year"},
	{"в", ""}, // «сегодня в 12:30»
}

// normalizeRussian replaces Russian date words; non-Cyrillic input is returned as is.
func normalizeRussian(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return unicode.Is(unicode.Cyrillic, r) }) {
		return s
	}
	var b strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); {
		if !unicode.IsLetter(rs[i]) {
			b.WriteRune(rs[i])
			i++
			continue
		}
		j := i
		for j < len(rs) && unicode.IsLetter(rs[j]) {
			j++
		}
		word := string(rs[i:j])
		lw := strings.ToLower(word)
		repl := word
		for _, w := range ruWords {
			if (w.prefix == "в" && lw == "в") || (w.prefix != "в" && strings.HasPrefix(lw, w.prefix)) {
				repl = w.en
				break
			}
		}
		b.WriteString(repl)
		i = j
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

var timeAgoRe = regexp.MustCompile(`\s*?([\d.]+)\s*?([^\d\s.]+)\s*?`)

// parseTimeAgo parses "2 hours 1 day ago", "5 минут назад".
func parseTimeAgo(s string, now time.Time) (time.Time, error) {
	s = strings.ToLower(normalizeRussian(s))
	if strings.Contains(s, "now") {
		return now, nil
	}
	for _, r := range []string{",", "ago", "and"} {
		s = strings.ReplaceAll(s, r, "")
	}
	var d time.Duration
	for _, m := range timeAgoRe.FindAllStringSubmatch(s, -1) {
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("timeago %q: %w", s, err)
		}
		unit := m[2]
		var u time.Duration
		switch {
		case strings.Contains(unit, "sec") || unit == "s":
			u = time.Second
		case strings.Contains(unit, "min") || unit == "m":
			u = time.Minute
		case strings.Contains(unit, "hour") || strings.Contains(unit, "hr") || unit == "h":
			u = time.Hour
		case strings.Contains(unit, "day") || unit == "d":
			u = 24 * time.Hour
		case strings.Contains(unit, "week") || strings.Contains(unit, "wk") || unit == "w":
			u = 7 * 24 * time.Hour
		case strings.Contains(unit, "month") || unit == "mo":
			u = 30 * 24 * time.Hour
		case strings.Contains(unit, "year") || unit == "y":
			u = 365 * 24 * time.Hour
		default:
			return time.Time{}, fmt.Errorf("timeago: unknown unit %q", unit)
		}
		d += time.Duration(v * float64(u))
	}
	return now.Add(-d), nil
}

var (
	agoRe       = regexp.MustCompile(`(?i)\bago`)
	todayRe     = regexp.MustCompile(`(?i)\btoday(?:[\s,]+(?:at)?\s*|[\s,]*|$)`)
	yesterdayRe = regexp.MustCompile(`(?i)\byesterday(?:[\s,]+(?:at)?\s*|[\s,]*|$)`)
	tomorrowRe  = regexp.MustCompile(`(?i)\btomorrow(?:[\s,]+(?:at)?\s*|[\s,]*|$)`)
	weekdayRe   = regexp.MustCompile(`(?i)\b(monday|tuesday|wednesday|thursday|friday|saturday|sunday)\s+at\s+`)
	noYearRe    = regexp.MustCompile(`^(\d{1,2}-\d{1,2})(\s|$)`)
	noYearRe2   = regexp.MustCompile(`^(\d{1,2}\s+\w{3})\s+(\d{1,2}:\d{1,2}.*)$`)
	allDigits   = regexp.MustCompile(`^\d+$`)
)

// parseUnknown is Prowlarr's DateTimeUtil.FromUnknown (used by fuzzytime and
// the date field).
func parseUnknown(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, l := range []string{dateLayout, time.RFC1123, time.RFC3339, time.RFC3339Nano} {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	if allDigits.MatchString(s) {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return time.Unix(n, 0), nil
		}
	}
	s = normalizeRussian(s)
	if strings.Contains(strings.ToLower(s), "now") {
		return now, nil
	}
	if agoRe.MatchString(s) {
		return parseTimeAgo(s, now)
	}
	// wall-clock arithmetic: adding durations would shift by an hour across DST
	at := func(days int, tod clock) time.Time {
		return time.Date(now.Year(), now.Month(), now.Day()+days, tod.h, tod.m, tod.s, 0, now.Location())
	}
	for _, rel := range []struct {
		re   *regexp.Regexp
		days int
	}{{todayRe, 0}, {yesterdayRe, -1}, {tomorrowRe, 1}} {
		if m := rel.re.FindString(s); m != "" {
			tod, err := parseTimeOfDay(strings.Replace(s, m, "", 1))
			if err != nil {
				return time.Time{}, err
			}
			return at(rel.days, tod), nil
		}
	}
	if m := weekdayRe.FindStringSubmatch(s); m != nil {
		tod, err := parseTimeOfDay(strings.Replace(s, m[0], "", 1))
		if err != nil {
			return time.Time{}, err
		}
		for back := 0; back < 7; back++ {
			if d := at(-back, tod); strings.EqualFold(d.Weekday().String(), m[1]) {
				return d, nil
			}
		}
	}
	if m := noYearRe.FindStringSubmatch(s); m != nil {
		s = strings.Replace(s, m[1], strconv.Itoa(now.Year())+"-"+m[1], 1)
	}
	if m := noYearRe2.FindStringSubmatch(s); m != nil {
		s = m[1] + " " + strconv.Itoa(now.Year()) + " " + m[2]
	}
	return parseFuzzy(s)
}

type clock struct{ h, m, s int }

func parseTimeOfDay(s string) (clock, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return clock{}, nil
	}
	for _, l := range []string{"15:04:05", "15:04", "3:04 PM", "3:04PM", "3:04:05 PM", "3PM", "3 PM"} {
		if t, err := time.Parse(l, strings.ToUpper(s)); err == nil {
			return clock{t.Hour(), t.Minute(), t.Second()}, nil
		}
	}
	return clock{}, fmt.Errorf("time of day %q not recognised", s)
}

// fuzzyLayouts replaces the DateTimeRoutines heuristics of Prowlarr with a
// list of the formats seen on trackers (US month/day order, as Prowlarr).
var fuzzyLayouts = []string{
	"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02",
	"2006-01-02 15:04:05 -0700", "2006-01-02 15:04:05 -07:00", "2006-01-02 15:04:05Z07:00",
	"2006/1/2 15:04:05", "2006/1/2 15:04", "2006/1/2",
	"2.1.2006 15:04:05", "2.1.2006 15:04", "2.1.2006", "2.1.06 15:04", "2.1.06",
	"1/2/2006 15:04:05", "1/2/2006 15:04", "1/2/2006 3:04:05 PM", "1/2/2006 3:04 PM", "1/2/2006", "1/2/06",
	"2-1-2006 15:04:05", "2-1-2006 15:04", "2-1-2006",
	"2 Jan 2006 15:04:05", "2 Jan 2006 15:04", "2 Jan 2006", "2 Jan 06 15:04", "2 Jan 06", "2-Jan-2006", "2-Jan-06",
	"2 January 2006 15:04", "2 January 2006", "2 January, 2006",
	"Jan 2, 2006 15:04:05", "Jan 2, 2006 15:04", "Jan 2, 2006 3:04 PM", "Jan 2, 2006", "Jan 2 2006", "January 2, 2006", "January 2 2006",
	"Mon, 2 Jan 2006 15:04:05 MST", "Mon, 2 Jan 2006 15:04:05 -0700", "Mon Jan 2 15:04:05 2006", "Mon, 2 Jan 2006",
	"Monday, January 2, 2006", "Monday, 2 January 2006",
	"Jan 2006", "January 2006",
}

func parseFuzzy(s string) (time.Time, error) {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, " ", " ")), " ")
	for _, l := range fuzzyLayouts {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("date %q not recognised", s)
}

// parseDateLayout implements the dateparse filter. layout is a Go layout or a
// .NET custom format ("dd MMM yy HH:mm:ss zzz"); like Prowlarr, a layout
// containing y/h/d is tried as .NET first.
func parseDateLayout(s, layout string) (time.Time, error) {
	s = strings.TrimSpace(normalizeRussian(s))
	if strings.ContainsAny(strings.ToLower(layout), "yhd") {
		if t, err := time.ParseInLocation(dotnetToGoLayout(layout), s, time.Local); err == nil {
			return t, nil
		}
	}
	t, err := time.ParseInLocation(layout, s, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("date %q does not match layout %q", s, layout)
	}
	return t, nil
}

// dotnetToGoLayout converts a .NET custom date format into a Go layout.
// Numeric tokens map to Go's non-padded forms, which also accept padded input.
func dotnetToGoLayout(f string) string {
	var b strings.Builder
	rs := []rune(f)
	for i := 0; i < len(rs); {
		c := rs[i]
		n := 1
		for i+n < len(rs) && rs[i+n] == c {
			n++
		}
		switch c {
		case 'y':
			if n <= 2 {
				b.WriteString("06")
			} else {
				b.WriteString("2006")
			}
		case 'M':
			switch {
			case n >= 4:
				b.WriteString("January")
			case n == 3:
				b.WriteString("Jan")
			default:
				b.WriteString("1")
			}
		case 'd':
			switch {
			case n >= 4:
				b.WriteString("Monday")
			case n == 3:
				b.WriteString("Mon")
			default:
				b.WriteString("2")
			}
		case 'H':
			b.WriteString("15")
		case 'h':
			b.WriteString("3")
		case 'm':
			b.WriteString("4")
		case 's':
			b.WriteString("5")
		case 'f', 'F':
			digits := strings.Repeat(map[rune]string{'f': "0", 'F': "9"}[c], n)
			if strings.HasSuffix(b.String(), ".") || strings.HasSuffix(b.String(), ",") {
				b.WriteString(digits) // "ss.fff" -> "5.000"
			} else {
				b.WriteString("." + digits)
			}
		case 't':
			b.WriteString("PM")
		case 'z':
			if n >= 3 {
				b.WriteString("-07:00")
			} else {
				b.WriteString("-07")
			}
		case 'K':
			b.WriteString("Z07:00")
		case '\'', '"':
			end := i + 1
			for end < len(rs) && rs[end] != c {
				end++
			}
			b.WriteString(string(rs[i+1 : min(end, len(rs))]))
			i = end + 1
			continue
		case '\\':
			if i+1 < len(rs) {
				b.WriteRune(rs[i+1])
			}
			i += 2
			continue
		default:
			b.WriteString(strings.Repeat(string(c), n))
		}
		i += n
	}
	return b.String()
}
