// Package filter parses and evaluates the episode filter language shared with the app (app/lib/filter).
package filter

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type Option struct {
	ID    string
	Label string
}

// Env is everything a filter needs besides the episode: the clock and the channel options.
type Env struct {
	Now      time.Time
	Loc      *time.Location
	Channels []Option
}

// Item is one episode as the filter sees it; zero Length, Published, Season and Number mean missing.
type Item struct {
	Title       string
	Channel     string
	ChannelName string
	Length      int64
	Published   int64
	Media       string
	Season      int64
	Number      int64
	Type        string
	InPlaylist  bool
	Position    int64
	Played      bool
	Starred     bool
	Downloaded  bool
	Tags        []string
}

type Error struct {
	Pos int
	Msg string
}

func (e *Error) Error() string { return fmt.Sprintf("%s (at %d)", e.Msg, e.Pos+1) }

// Span marks a stored-by-id value or a tag inside the source, for rewriting.
type Span struct {
	Start, End int
	Kind       string
	ID         string
}

type Filter struct {
	root           node
	Spans          []Span
	UsesPlaylist   bool
	UsesDownloaded bool
}

func (f *Filter) Match(it *Item) bool { return f.root == nil || f.root.eval(it) }

type kind int

const (
	kText kind = iota
	kChoice
	kDuration
	kDateTime
	kNumber
)

type keyDef struct {
	name    string
	kind    kind
	options []Option
}

var boolKeys = []string{"unplayed", "played", "inprogress", "starred", "downloaded", "playlist"}

var (
	mediaOptions = []Option{{"audio", "audio"}, {"video", "video"}}
	typeOptions  = []Option{{"full", "full"}, {"trailer", "trailer"}, {"bonus", "bonus"}}
)

func (e *Env) key(name string) (keyDef, bool) {
	switch name {
	case "title":
		return keyDef{name, kText, nil}, true
	case "title.length", "season", "number":
		return keyDef{name, kNumber, nil}, true
	case "channel", "podcast":
		return keyDef{"channel", kChoice, e.Channels}, true
	case "length", "left":
		return keyDef{name, kDuration, nil}, true
	case "published":
		return keyDef{name, kDateTime, nil}, true
	case "media":
		return keyDef{name, kChoice, mediaOptions}, true
	case "type":
		return keyDef{name, kChoice, typeOptions}, true
	}
	return keyDef{}, false
}

// Parse compiles src; errors point at the rune where they start.
func Parse(src string, env Env) (*Filter, *Error) {
	if env.Loc == nil {
		env.Loc = time.UTC
	}
	p := &parser{src: []rune(src), env: &env, f: &Filter{}}
	p.ws()
	if p.eof() {
		return p.f, nil
	}
	n, err := p.or()
	if err != nil {
		return nil, err
	}
	p.ws()
	if !p.eof() {
		if p.peek() == ')' {
			return nil, p.errAt(p.pos, "unexpected )")
		}
		return nil, p.errAt(p.pos, "expected AND or OR")
	}
	p.f.root = n
	return p.f, nil
}

type parser struct {
	src []rune
	pos int
	env *Env
	f   *Filter
}

func (p *parser) eof() bool  { return p.pos >= len(p.src) }
func (p *parser) peek() rune { return p.src[p.pos] }
func (p *parser) ws() {
	for !p.eof() && unicode.IsSpace(p.peek()) {
		p.pos++
	}
}
func (p *parser) errAt(pos int, msg string, args ...any) *Error {
	return &Error{Pos: pos, Msg: fmt.Sprintf(msg, args...)}
}

// keyword reports whether the next word is exactly kw.
func (p *parser) keyword(kw string) bool {
	end := p.pos + len(kw)
	if end > len(p.src) || !strings.EqualFold(string(p.src[p.pos:end]), kw) {
		return false
	}
	return end == len(p.src) || unicode.IsSpace(p.src[end]) || p.src[end] == '(' || p.src[end] == ')'
}

func (p *parser) or() (node, *Error) {
	left, err := p.and()
	if err != nil {
		return nil, err
	}
	for {
		p.ws()
		if !p.keyword("OR") {
			return left, nil
		}
		p.pos += 2
		right, err := p.and()
		if err != nil {
			return nil, err
		}
		left = orNode{left, right}
	}
}

func (p *parser) and() (node, *Error) {
	left, err := p.unary()
	if err != nil {
		return nil, err
	}
	for {
		p.ws()
		if p.eof() || p.peek() == ')' || p.keyword("OR") {
			return left, nil
		}
		if p.keyword("AND") {
			p.pos += 3
		}
		right, err := p.unary()
		if err != nil {
			return nil, err
		}
		left = andNode{left, right}
	}
}

func (p *parser) unary() (node, *Error) {
	p.ws()
	if !p.eof() && p.peek() == '!' {
		p.pos++
		n, err := p.unary()
		if err != nil {
			return nil, err
		}
		return notNode{n}, nil
	}
	return p.primary()
}

func isWordRune(r rune) bool {
	return !unicode.IsSpace(r) && !strings.ContainsRune(`()"`, r)
}

func isNameRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-'
}

func (p *parser) primary() (node, *Error) {
	p.ws()
	if p.eof() {
		return nil, p.errAt(p.pos, "expected a filter term")
	}
	start := p.pos
	switch c := p.peek(); {
	case c == '(':
		p.pos++
		p.ws()
		if !p.eof() && p.peek() == ')' {
			return nil, p.errAt(p.pos, "expected a filter term")
		}
		n, err := p.or()
		if err != nil {
			return nil, err
		}
		p.ws()
		if p.eof() || p.peek() != ')' {
			return nil, p.errAt(p.pos, "expected )")
		}
		p.pos++
		return n, nil
	case c == ')':
		return nil, p.errAt(p.pos, "expected a filter term")
	case c == '$':
		return p.comparison()
	case c == '#':
		p.pos++
		ns := p.pos
		for !p.eof() && (isNameRune(p.peek()) || p.peek() == '.') {
			p.pos++
		}
		name := strings.ToLower(string(p.src[ns:p.pos]))
		if name == "" {
			return nil, p.errAt(start, "expected a name after #")
		}
		p.f.Spans = append(p.f.Spans, Span{Start: ns, End: p.pos, Kind: "tag", ID: name})
		return tagNode{name}, nil
	}
	if p.keyword("AND") || p.keyword("OR") {
		return nil, p.errAt(start, "expected a filter term")
	}
	return nil, p.errAt(start, `expected $key, #tag, ! or ( (text goes in $title ~ "words")`)
}

func (p *parser) quoted() (string, *Error) {
	start := p.pos
	p.pos++
	for !p.eof() && p.peek() != '"' {
		p.pos++
	}
	if p.eof() {
		return "", p.errAt(start, "unclosed quote")
	}
	s := string(p.src[start+1 : p.pos])
	p.pos++
	return s, nil
}

var operators = []string{"!=", ">=", "<=", "^:", "$:", "=", ":", ">", "<", "~"}

func (p *parser) comparison() (node, *Error) {
	start := p.pos
	p.pos++
	ns := p.pos
	for !p.eof() && (isNameRune(p.peek()) || p.peek() == '.') {
		p.pos++
	}
	name := strings.ToLower(string(p.src[ns:p.pos]))
	if name == "" {
		return nil, p.errAt(start, "expected a name after $")
	}
	flag := slices.Contains(boolKeys, name)
	k, ok := p.env.key(name)
	if !ok && !flag {
		return nil, p.errAt(start, "unknown key $%s", name)
	}
	// $podcast is saved as $channel, the only name older apps read.
	if name == "podcast" {
		p.f.Spans = append(p.f.Spans, Span{Start: ns, End: p.pos, Kind: "alias", ID: "channel"})
	}
	nameEnd := p.pos
	p.ws()
	opPos := p.pos
	op := ""
	for _, o := range operators {
		end := p.pos + len([]rune(o))
		if end <= len(p.src) && string(p.src[p.pos:end]) == o {
			op = o
			p.pos = end
			break
		}
	}
	if flag {
		if op != "" {
			return nil, p.errAt(opPos, "$%s is yes or no: write $%s or !$%s", name, name, name)
		}
		p.pos = nameEnd
		p.f.UsesDownloaded = p.f.UsesDownloaded || name == "downloaded"
		p.f.UsesPlaylist = p.f.UsesPlaylist || name == "playlist"
		return boolNode{name}, nil
	}
	if op == "" {
		p.pos = nameEnd
		return hasNode{k.name}, nil
	}
	if op == ":" {
		op = "="
	}
	allowed := map[kind]string{
		kText: "= != > < >= <= ~ ^: $:", kChoice: "= != > < >= <=",
		kNumber: "= != > < >= <=", kDuration: "= != > < >= <=", kDateTime: "= != > < >= <=",
	}[k.kind]
	if !strings.Contains(" "+allowed+" ", " "+op+" ") {
		return nil, p.errAt(opPos, "%s can't be used with $%s", op, name)
	}
	p.ws()
	if p.eof() || p.peek() == ')' {
		return nil, p.errAt(p.pos, "expected a value after %s", op)
	}
	switch k.kind {
	case kText:
		valPos := p.pos
		var s string
		if p.peek() == '"' {
			var err *Error
			if s, err = p.quoted(); err != nil {
				return nil, err
			}
		} else {
			for !p.eof() && isWordRune(p.peek()) {
				p.pos++
			}
			s = string(p.src[valPos:p.pos])
		}
		return textCmp{op: op, val: strings.ToLower(s)}, nil
	case kChoice:
		var vals []int
		for {
			p.ws()
			idx, err := p.choice(k)
			if err != nil {
				return nil, err
			}
			vals = append(vals, idx)
			save := p.pos
			p.ws()
			if p.eof() || p.peek() != ',' {
				p.pos = save
				break
			}
			p.pos++
		}
		if len(vals) > 1 && op != "=" && op != "!=" {
			return nil, p.errAt(opPos, "use one value with %s", op)
		}
		return choiceCmp{key: k.name, op: op, vals: vals, options: k.options}, nil
	}
	valPos := p.pos
	v, err := p.expr()
	if err != nil {
		return nil, err
	}
	switch k.kind {
	case kNumber:
		if v.t != tNum {
			return nil, p.errAt(valPos, "expected a number")
		}
		return numCmp{key: k.name, op: op, lo: v.a, hi: v.a}, nil
	case kDuration:
		if v.t != tDur {
			return nil, p.errAt(valPos, "expected a duration like 30min")
		}
		return numCmp{key: k.name, op: op, lo: v.a, hi: v.a}, nil
	}
	switch v.t {
	case tDur:
		v = p.addDur(p.today(), v.a)
	case tNum:
		return nil, p.errAt(valPos, "expected a date like 2026-10-07, today or -7d")
	}
	if v.t == tDate {
		return numCmp{key: k.name, op: op, lo: v.a, hi: v.b, day: true}, nil
	}
	return numCmp{key: k.name, op: op, lo: v.a, hi: v.a}, nil
}

// choice reads one value and resolves it to an option index; -1 is a stored id that no longer exists.
func (p *parser) choice(k keyDef) (int, *Error) {
	start := p.pos
	if p.eof() || p.peek() == ')' || p.peek() == ',' {
		return 0, p.errAt(p.pos, "expected a value")
	}
	var word string
	if p.peek() == '"' {
		s, err := p.quoted()
		if err != nil {
			return 0, err
		}
		word = s
	} else {
		for !p.eof() && isWordRune(p.peek()) && p.peek() != ',' {
			p.pos++
		}
		word = string(p.src[start:p.pos])
	}
	dynamic := k.name == "channel"
	if dynamic && strings.HasPrefix(word, "@") && p.src[start] == '@' {
		id := word[1:]
		p.f.Spans = append(p.f.Spans, Span{Start: start, End: p.pos, Kind: k.name, ID: id})
		for i, o := range k.options {
			if o.ID == id {
				return i, nil
			}
		}
		return -1, nil
	}
	lw := strings.ToLower(word)
	exact, prefix := -1, -1
	exactN, prefixN := 0, 0
	for i, o := range k.options {
		ll := strings.ToLower(o.Label)
		if ll == lw {
			exact, exactN = i, exactN+1
		} else if strings.HasPrefix(ll, lw) {
			prefix, prefixN = i, prefixN+1
		}
	}
	idx := -1
	switch {
	case exactN > 1:
		return 0, p.errAt(start, "%q matches more than one option: pick one from the list", word)
	case exactN == 1:
		idx = exact
	case prefixN > 1:
		return 0, p.errAt(start, "%q matches more than one option: pick one from the list", word)
	case prefixN == 1:
		idx = prefix
	default:
		return 0, p.errAt(start, "no option %q for $%s", word, k.name)
	}
	if dynamic {
		p.f.Spans = append(p.f.Spans, Span{Start: start, End: p.pos, Kind: k.name, ID: k.options[idx].ID})
	}
	return idx, nil
}

type vtype int

const (
	tNum vtype = iota
	tDur
	tDate
	tTime
)

var typeNames = map[vtype]string{tNum: "number", tDur: "duration", tDate: "date", tTime: "date"}

// value is a number, a duration in seconds, a day [a, b) or a moment a, all in unix seconds.
type value struct {
	t    vtype
	a, b float64
}

func (p *parser) expr() (value, *Error) {
	left, err := p.term()
	if err != nil {
		return value{}, err
	}
	for {
		save := p.pos
		p.ws()
		if p.eof() || (p.peek() != '+' && p.peek() != '-') || p.pos+1 >= len(p.src) || !unicode.IsSpace(p.src[p.pos+1]) {
			p.pos = save
			return left, nil
		}
		opPos := p.pos
		op := p.peek()
		p.pos++
		right, err := p.term()
		if err != nil {
			return value{}, err
		}
		if left, err = p.arith(opPos, op, left, right); err != nil {
			return value{}, err
		}
	}
}

func (p *parser) term() (value, *Error) {
	left, err := p.factor()
	if err != nil {
		return value{}, err
	}
	for {
		save := p.pos
		p.ws()
		if p.eof() || (p.peek() != '*' && p.peek() != '/') {
			p.pos = save
			return left, nil
		}
		opPos := p.pos
		op := p.peek()
		p.pos++
		right, err := p.factor()
		if err != nil {
			return value{}, err
		}
		if left, err = p.arith(opPos, op, left, right); err != nil {
			return value{}, err
		}
	}
}

func (p *parser) factor() (value, *Error) {
	p.ws()
	if !p.eof() && (p.peek() == '-' || p.peek() == '+') {
		opPos := p.pos
		neg := p.peek() == '-'
		p.pos++
		v, err := p.factor()
		if err != nil {
			return value{}, err
		}
		if v.t != tNum && v.t != tDur {
			return value{}, p.errAt(opPos, "expected a number or a duration")
		}
		if neg {
			v.a = -v.a
		}
		return v, nil
	}
	return p.literal()
}

func (p *parser) arith(pos int, op rune, l, r value) (value, *Error) {
	bad := func() (value, *Error) {
		return value{}, p.errAt(pos, "can't combine %s and %s with %c", typeNames[l.t], typeNames[r.t], op)
	}
	switch op {
	case '+', '-':
		sign := 1.0
		if op == '-' {
			sign = -1
		}
		switch {
		case l.t == tNum && r.t == tNum, l.t == tDur && r.t == tDur:
			return value{t: l.t, a: l.a + sign*r.a}, nil
		case l.t == tDate && r.t == tDur:
			return p.addDur(l, sign*r.a), nil
		case l.t == tTime && r.t == tDur:
			return value{t: tTime, a: l.a + sign*r.a}, nil
		case l.t == tDur && r.t == tDate && op == '+':
			return p.addDur(r, l.a), nil
		}
		return bad()
	case '*':
		switch {
		case l.t == tNum && r.t == tNum, l.t == tDur && r.t == tNum:
			return value{t: l.t, a: l.a * r.a}, nil
		case l.t == tNum && r.t == tDur:
			return value{t: tDur, a: l.a * r.a}, nil
		}
		return bad()
	}
	if r.t != tNum || (l.t != tNum && l.t != tDur) {
		return bad()
	}
	if r.a == 0 {
		return value{}, p.errAt(pos, "division by zero")
	}
	return value{t: l.t, a: l.a / r.a}, nil
}

// addDur moves a day by whole days on the calendar, or turns it into a moment otherwise.
func (p *parser) addDur(d value, secs float64) value {
	if math.Mod(secs, 86400) == 0 {
		t := time.Unix(int64(d.a), 0).In(p.env.Loc)
		return p.day(t.Year(), int(t.Month()), t.Day()+int(secs/86400))
	}
	return value{t: tTime, a: d.a + secs}
}

func (p *parser) day(y, m, d int) value {
	s := time.Date(y, time.Month(m), d, 0, 0, 0, 0, p.env.Loc)
	e := time.Date(y, time.Month(m), d+1, 0, 0, 0, 0, p.env.Loc)
	return value{t: tDate, a: float64(s.Unix()), b: float64(e.Unix())}
}

func (p *parser) today() value {
	n := p.env.Now.In(p.env.Loc)
	return p.day(n.Year(), int(n.Month()), n.Day())
}

func (p *parser) word() string {
	start := p.pos
	for !p.eof() && (unicode.IsLetter(p.peek()) || unicode.IsDigit(p.peek()) || strings.ContainsRune(":._", p.peek()) ||
		(p.peek() == '-' && p.pos > start)) {
		p.pos++
	}
	return string(p.src[start:p.pos])
}

// peekWord returns the next word without consuming it, and where it ends.
func (p *parser) peekWord() (string, int) {
	save := p.pos
	p.ws()
	w := p.word()
	end := p.pos
	p.pos = save
	return strings.ToLower(w), end
}

var units = map[string]float64{
	"s": 1, "sec": 1, "secs": 1, "second": 1, "seconds": 1,
	"min": 60, "mins": 60, "minute": 60, "minutes": 60,
	"h": 3600, "hr": 3600, "hrs": 3600, "hour": 3600, "hours": 3600,
	"d": 86400, "day": 86400, "days": 86400,
	"w": 604800, "wk": 604800, "week": 604800, "weeks": 604800,
	"mo": 2592000, "month": 2592000, "months": 2592000,
	"y": 31536000, "yr": 31536000, "year": 31536000, "years": 31536000,
}

var numberRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

var months = []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
var weekdays = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

func prefixIndex(list []string, w string) int {
	if len(w) < 3 {
		return -1
	}
	for i, x := range list {
		if strings.HasPrefix(w, x) {
			full := map[string]string{"jan": "january", "feb": "february", "mar": "march", "apr": "april", "may": "may", "jun": "june",
				"jul": "july", "aug": "august", "sep": "september", "oct": "october", "nov": "november", "dec": "december",
				"sun": "sunday", "mon": "monday", "tue": "tuesday", "wed": "wednesday", "thu": "thursday", "fri": "friday", "sat": "saturday"}[x]
			if strings.HasPrefix(full, w) || (x == "sep" && w == "sept") || (x == "tue" && w == "tues") || (x == "thu" && (w == "thur" || w == "thurs")) {
				return i
			}
		}
	}
	return -1
}

func (p *parser) literal() (value, *Error) {
	p.ws()
	start := p.pos
	w := strings.ToLower(p.word())
	if w == "" {
		if p.eof() {
			return value{}, p.errAt(start, "expected a value")
		}
		return value{}, p.errAt(start, "expected a value")
	}
	if n, err := strconv.ParseFloat(w, 64); err == nil && numberRe.MatchString(w) {
		next, end := p.peekWord()
		if u, ok := units[next]; ok {
			p.pos = end
			return p.ago(value{t: tDur, a: n * u}), nil
		}
		if next == "m" {
			return value{}, p.errAt(start, `"m" is ambiguous: use min or mo`)
		}
		if mi := prefixIndex(months, next); mi >= 0 && n == math.Trunc(n) && n >= 1 && n <= 31 {
			p.pos = end
			return p.day(p.env.Now.In(p.env.Loc).Year(), mi+1, int(n)), nil
		}
		return value{t: tNum, a: n}, nil
	}
	if d, ok, ambiguous := compactDuration(w); ok {
		return p.ago(value{t: tDur, a: d}), nil
	} else if ambiguous {
		return value{}, p.errAt(start, `"m" is ambiguous: use min or mo`)
	}
	now := p.env.Now.In(p.env.Loc)
	switch w {
	case "today":
		return p.today(), nil
	case "yesterday":
		return p.day(now.Year(), int(now.Month()), now.Day()-1), nil
	case "tomorrow":
		return p.day(now.Year(), int(now.Month()), now.Day()+1), nil
	case "now":
		return value{t: tTime, a: float64(p.env.Now.Unix())}, nil
	case "next", "last":
		next, end := p.peekWord()
		if wd := prefixIndex(weekdays, next); wd >= 0 {
			p.pos = end
			delta := (wd - int(now.Weekday()) + 7) % 7
			if w == "next" {
				if delta == 0 {
					delta = 7
				}
			} else {
				delta -= 7
			}
			return p.day(now.Year(), int(now.Month()), now.Day()+delta), nil
		}
		return value{}, p.errAt(start, "expected a value")
	}
	if wd := prefixIndex(weekdays, w); wd >= 0 {
		delta := (int(now.Weekday()) - wd + 7) % 7
		return p.day(now.Year(), int(now.Month()), now.Day()-delta), nil
	}
	if mi := prefixIndex(months, w); mi >= 0 {
		next, end := p.peekWord()
		if n, err := strconv.Atoi(next); err == nil && n >= 1 && n <= 31 {
			p.pos = end
			return p.day(now.Year(), mi+1, n), nil
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", w, p.env.Loc); err == nil {
		return p.day(t.Year(), int(t.Month()), t.Day()), nil
	}
	for _, layout := range []string{"2006-01-02t15:04", "2006-01-02t15:04:05"} {
		if t, err := time.ParseInLocation(layout, w, p.env.Loc); err == nil {
			return value{t: tTime, a: float64(t.Unix())}, nil
		}
	}
	return value{}, p.errAt(start, "%q is not a number, duration or date", w)
}

// ago turns "3 days ago" into a negative duration.
func (p *parser) ago(v value) value {
	if next, end := p.peekWord(); next == "ago" {
		p.pos = end
		v.a = -v.a
	}
	return v
}

// compactDuration reads forms like 30min, 2h, 1h30min and 6mo.
func compactDuration(w string) (secs float64, ok, ambiguous bool) {
	i := 0
	parts := 0
	for i < len(w) {
		j := i
		for j < len(w) && (w[j] >= '0' && w[j] <= '9' || w[j] == '.') {
			j++
		}
		if j == i {
			return 0, false, false
		}
		n, err := strconv.ParseFloat(w[i:j], 64)
		if err != nil {
			return 0, false, false
		}
		k := j
		for k < len(w) && w[k] >= 'a' && w[k] <= 'z' {
			k++
		}
		u := w[j:k]
		if u == "m" {
			return 0, false, true
		}
		mult, known := units[u]
		if !known {
			return 0, false, false
		}
		secs += n * mult
		parts++
		i = k
	}
	return secs, parts > 0, false
}

type node interface{ eval(*Item) bool }

type andNode struct{ a, b node }
type orNode struct{ a, b node }
type notNode struct{ n node }
type tagNode struct{ name string }
type boolNode struct{ name string }
type hasNode struct{ key string }
type textCmp struct {
	op  string
	val string
}
type choiceCmp struct {
	key     string
	op      string
	vals    []int
	options []Option
}
type numCmp struct {
	key    string
	op     string
	lo, hi float64
	day    bool
}

func (n andNode) eval(it *Item) bool { return n.a.eval(it) && n.b.eval(it) }
func (n orNode) eval(it *Item) bool  { return n.a.eval(it) || n.b.eval(it) }
func (n notNode) eval(it *Item) bool { return !n.n.eval(it) }

func (n tagNode) eval(it *Item) bool {
	for _, t := range it.Tags {
		if t == n.name {
			return true
		}
	}
	return false
}

func (n boolNode) eval(it *Item) bool {
	switch n.name {
	case "unplayed":
		return !it.Played
	case "played":
		return it.Played
	case "inprogress":
		return it.Position > 0 && !it.Played
	case "starred":
		return it.Starred
	case "playlist":
		return it.InPlaylist
	}
	return it.Downloaded
}

func (n hasNode) eval(it *Item) bool {
	_, ok := numberOf(n.key, it)
	if n.key == "title" || n.key == "channel" || n.key == "media" || n.key == "type" || n.key == "title.length" {
		return true
	}
	return ok
}

func numberOf(key string, it *Item) (float64, bool) {
	switch key {
	case "title.length":
		return float64(len([]rune(it.Title))), true
	case "season":
		return float64(it.Season), it.Season != 0
	case "number":
		return float64(it.Number), it.Number != 0
	case "length":
		return float64(it.Length), it.Length > 0
	case "left":
		return float64(max(it.Length-it.Position, 0)), it.Length > 0
	case "published":
		return float64(it.Published), it.Published != 0
	}
	return 0, false
}

func compare(op string, c int) bool {
	switch op {
	case "=":
		return c == 0
	case "!=":
		return c != 0
	case ">":
		return c > 0
	case "<":
		return c < 0
	case ">=":
		return c >= 0
	}
	return c <= 0
}

func (n textCmp) eval(it *Item) bool {
	t := strings.ToLower(it.Title)
	switch n.op {
	case "~":
		return strings.Contains(t, n.val)
	case "^:":
		return strings.HasPrefix(t, n.val)
	case "$:":
		return strings.HasSuffix(t, n.val)
	}
	return compare(n.op, strings.Compare(t, n.val))
}

func (n choiceCmp) eval(it *Item) bool {
	var cur string
	switch n.key {
	case "channel":
		cur = it.Channel
	case "media":
		cur = it.Media
	case "type":
		cur = it.Type
		if cur == "" {
			cur = "full"
		}
	}
	ci := -1
	for i, o := range n.options {
		if o.ID == cur {
			ci = i
			break
		}
	}
	if n.op == "=" || n.op == "!=" {
		hit := false
		for _, v := range n.vals {
			if v >= 0 && v == ci {
				hit = true
			}
		}
		return hit == (n.op == "=")
	}
	if ci < 0 || n.vals[0] < 0 {
		return false
	}
	return compare(n.op, ci-n.vals[0])
}

func (n numCmp) eval(it *Item) bool {
	v, ok := numberOf(n.key, it)
	if !ok {
		return false
	}
	if n.day {
		switch n.op {
		case "=":
			return v >= n.lo && v < n.hi
		case "!=":
			return v < n.lo || v >= n.hi
		case ">":
			return v >= n.hi
		case ">=":
			return v >= n.lo
		case "<":
			return v < n.lo
		}
		return v < n.hi
	}
	c := 0
	if v < n.lo {
		c = -1
	} else if v > n.lo {
		c = 1
	}
	return compare(n.op, c)
}

// Rewrite replaces each span's text with what fn returns for it, leaving the rest untouched.
func Rewrite(src string, spans []Span, fn func(Span) (string, bool)) string {
	r := []rune(src)
	var b strings.Builder
	last := 0
	for _, s := range spans {
		repl, ok := fn(s)
		if !ok || s.Start < last {
			continue
		}
		b.WriteString(string(r[last:s.Start]))
		b.WriteString(repl)
		last = s.End
	}
	b.WriteString(string(r[last:]))
	return b.String()
}

// Quote writes a label so it reads back as one value.
func Quote(label string) string {
	for _, c := range label {
		if !isNameRune(c) {
			return `"` + strings.ReplaceAll(label, `"`, "") + `"`
		}
	}
	if label == "" || strings.EqualFold(label, "AND") || strings.EqualFold(label, "OR") {
		return `"` + label + `"`
	}
	return label
}

// ToStored swaps every channel label for its id, so renames never break the filter, and $podcast for $channel.
func ToStored(src string, env Env) (string, *Error) {
	f, err := Parse(src, env)
	if err != nil {
		return "", err
	}
	return Rewrite(src, f.Spans, func(s Span) (string, bool) {
		if s.Kind == "tag" {
			return "", false
		}
		if s.Kind == "alias" {
			return s.ID, true
		}
		return "@" + s.ID, true
	}), nil
}

// ToDisplay swaps stored ids back for their current labels.
func ToDisplay(stored string, env Env) string {
	f, err := Parse(stored, env)
	if err != nil {
		return stored
	}
	labels := map[string]string{}
	for _, o := range env.Channels {
		labels[o.ID] = o.Label
	}
	return Rewrite(stored, f.Spans, func(s Span) (string, bool) {
		if s.Kind == "tag" || s.Kind == "alias" {
			return "", false
		}
		l, ok := labels[s.ID]
		return Quote(l), ok
	})
}

// RenameTag rewrites #from to #to wherever it is used as a tag.
func RenameTag(stored, from, to string, env Env) string {
	f, err := Parse(stored, env)
	if err != nil {
		return stored
	}
	return Rewrite(stored, f.Spans, func(s Span) (string, bool) {
		return to, s.Kind == "tag" && s.ID == from
	})
}
