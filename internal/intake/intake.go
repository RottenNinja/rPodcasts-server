// Package intake turns every form and API body into a typed, validated request; nothing else reads raw input.
package intake

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"podcasts/internal/feed"
	"podcasts/internal/store"
)

// Values is what forms and decoded API bodies both offer.
type Values interface {
	Get(key string) string
	Has(key string) bool
}

func str(v Values, k string) string { return strings.TrimSpace(v.Get(k)) }

func checked(v Values, k string) bool {
	s := strings.ToLower(str(v, k))
	return s == "1" || s == "on" || s == "true" || s == "yes"
}

// num reads an optional whole number within [lo, hi]; empty gives 0.
func num(v Values, k, label string, lo, hi int, errs *[]error) int {
	s := str(v, k)
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < lo || n > hi {
		*errs = append(*errs, fmt.Errorf("%s: a whole number from %d to %d", label, lo, hi))
		return 0
	}
	return n
}

func required(v Values, k, label string, lo, hi int, errs *[]error) int {
	if str(v, k) == "" {
		*errs = append(*errs, fmt.Errorf("%s is required", label))
		return 0
	}
	return num(v, k, label, lo, hi, errs)
}

// NormalizeURL reads podcast-app schemes and bare addresses as https and lower-cases the host.
func NormalizeURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("address is required")
	}
	low := strings.ToLower(s)
	for _, p := range []string{"pcast://", "itpc://", "feed://", "podcast://", "podcasts://", "itms-pcast://", "itms-podcasts://"} {
		if strings.HasPrefix(low, p) {
			s = "https://" + s[len(p):]
			low = strings.ToLower(s)
			break
		}
	}
	if strings.HasPrefix(low, "feed:") && !strings.HasPrefix(low, "feed://") {
		s = s[5:]
		low = strings.ToLower(s)
	}
	if !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "https://") {
		if scheme, rest, ok := strings.Cut(s, "://"); ok {
			// Any other scheme, such as folder://, is the downloader engine's to read, so it is kept as written.
			u, err := url.Parse(s)
			if err != nil || u.Scheme == "" || !strings.EqualFold(u.Scheme, scheme) || strings.TrimLeft(rest, "/") == "" {
				return "", errors.New("address: not a valid address")
			}
			return strings.ToLower(scheme) + "://" + rest, nil
		}
		s = "https://" + strings.TrimPrefix(s, "//")
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || strings.ContainsAny(u.Host, " \t") {
		return "", errors.New("address: not a valid web address")
	}
	u.Host = strings.ToLower(u.Host)
	u.Scheme = strings.ToLower(u.Scheme)
	u.Fragment = ""
	if u.User != nil {
		return "", errors.New("address: put the user name and password in their own fields")
	}
	return u.String(), nil
}

type Follow struct {
	URL string
	// Title is the listener's own name for the channel; empty takes the address's own.
	Title    string
	Tags     []string
	Username string
	Password string
}

func ParseFollow(v Values) (Follow, error) {
	var errs []error
	u, err := NormalizeURL(v.Get("url"))
	if err != nil {
		errs = append(errs, err)
	}
	f := Follow{URL: u, Title: text(v, "title", "name", 200, &errs), Tags: tags(v.Get("tags"), &errs), Username: str(v, "username"), Password: v.Get("password")}
	if len(f.Username) > 200 || len(f.Password) > 200 {
		errs = append(errs, errors.New("user name and password: at most 200 characters"))
	}
	return f, errors.Join(errs...)
}

// AddPage is what the Add a channel page is opened with: its tab, the search words, an address to start from and a tag.
type AddPage struct {
	Manual bool
	Term   string
	URL    string
	Tag    string
}

func ParseAddPage(v Values) (AddPage, error) {
	var errs []error
	p := AddPage{Manual: str(v, "tab") == "manual", Term: text(v, "q", "search", 200, &errs), URL: text(v, "url", "address", 2000, &errs),
		Tag: strings.Join(store.NormalizeTags(v.Get("tag")), " ")}
	if len(errs) > 0 {
		p.Term, p.URL = "", ""
	}
	return p, errors.Join(errs...)
}

// PreviewPage is an address to show before following it, and the search it was picked from.
type PreviewPage struct {
	Follow
	Term string
}

func ParsePreviewPage(v Values) (PreviewPage, error) {
	f, err := ParseFollow(v)
	errs := []error{err}
	return PreviewPage{Follow: f, Term: text(v, "q", "search", 200, &errs)}, errors.Join(errs...)
}

// ParseSearch reads the words of a directory search.
func ParseSearch(v Values) (string, error) {
	var errs []error
	term := text(v, "q", "search", 200, &errs)
	if term == "" {
		errs = append(errs, errors.New("search: type a podcast's name"))
	}
	return term, errors.Join(errs...)
}

func text(v Values, k, label string, maxLen int, errs *[]error) string {
	s := str(v, k)
	if utf8.RuneCountInString(s) > maxLen {
		*errs = append(*errs, fmt.Errorf("%s: at most %d characters", label, maxLen))
	}
	return s
}

func tags(raw string, errs *[]error) []string {
	t := store.NormalizeTags(raw)
	for _, tag := range t {
		for _, r := range tag {
			if !(r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r > 127) {
				*errs = append(*errs, fmt.Errorf("tag %q: use letters, digits, - and _ only", tag))
				return nil
			}
		}
		if len(tag) > 40 {
			*errs = append(*errs, fmt.Errorf("tag %q: at most 40 characters", tag))
		}
	}
	return t
}

var qualities = map[string]bool{"audio": true, "480": true, "720": true, "1080": true}

// quality reads a video quality; empty stays empty, which means the default.
func quality(v Values, errs *[]error) string {
	q := str(v, "quality")
	if q != "" && !qualities[q] {
		*errs = append(*errs, errors.New("video quality: audio only, 480p, 720p or 1080p"))
	}
	return q
}

func ParseChannelSettings(v Values) (store.ChannelSettings, error) {
	var errs []error
	c := store.ChannelSettings{
		Title:      text(v, "title", "name", 200, &errs),
		Tags:       tags(v.Get("tags"), &errs),
		Speed:      num(v, "speed", "speed", 50, 300, &errs),
		SkipStart:  num(v, "skip_start", "skip the first", 0, 3600, &errs),
		SkipEnd:    num(v, "skip_end", "skip the last", 0, 3600, &errs),
		Quality:    quality(v, &errs),
		CheckEvery: num(v, "check_every", "check every", 15, 10080, &errs),
		FetchCount: num(v, "fetch_count", "how many to fetch", 1, 1000, &errs),
		Keep:       num(v, "keep", "keep in the database", 1, 10000, &errs),
		Username:   str(v, "username"),
		Password:   v.Get("password"),
	}
	if c.Speed == 0 {
		c.Speed = 100
	}
	c.KeepPass = c.Password == "" && !checked(v, "clear_password")
	return c, errors.Join(errs...)
}

type ChannelSpeed struct{ Speed int }

func ParseChannelSpeed(v Values) (ChannelSpeed, error) {
	var errs []error
	return ChannelSpeed{Speed: required(v, "speed", "speed", 50, 300, &errs)}, errors.Join(errs...)
}

type ReadBack struct{ Depth int }

func ParseReadBack(v Values, def int) (ReadBack, error) {
	var errs []error
	d := num(v, "depth", "how many episodes", 1, 5000, &errs)
	if d == 0 {
		d = def
	}
	return ReadBack{Depth: d}, errors.Join(errs...)
}

var Sorts = []struct{ Key, Label string }{
	{"newest", "Newest first"}, {"oldest", "Oldest first"}, {"shortest", "Shortest first"}, {"longest", "Longest first"}, {"manual", "In my order"}, {"shuffle", "Shuffled"},
}

func validSort(s string) bool {
	for _, x := range Sorts {
		if x.Key == s {
			return true
		}
	}
	return false
}

// Playlist carries the filter as typed; the caller checks it against the filter language.
type Playlist struct {
	Name     string
	Filter   string
	Sort     string
	Autoplay bool
	// hidePlayed is unsent by an app from before Hide played episodes.
	hidePlayed, hideUnsent bool
}

// HidePlayed is the setting as sent, or the saved one when it was not sent.
func (p Playlist) HidePlayed(saved bool) bool {
	if p.hideUnsent {
		return saved
	}
	return p.hidePlayed
}

func ParsePlaylist(v Values) (Playlist, error) {
	var errs []error
	p := Playlist{Name: text(v, "name", "name", 120, &errs), Filter: text(v, "filter", "filter", 2000, &errs), Sort: str(v, "sort"), Autoplay: checked(v, "autoplay"),
		hidePlayed: checked(v, "hide_played"), hideUnsent: !v.Has("hide_played")}
	if p.Name == "" {
		errs = append(errs, errors.New("name is required"))
	}
	if p.Sort == "" {
		p.Sort = "newest"
	}
	if !validSort(p.Sort) {
		errs = append(errs, errors.New("show: pick an order from the list"))
	}
	return p, errors.Join(errs...)
}

func ParseServerSettings(v Values) (store.ServerSettings, error) {
	var errs []error
	s := store.ServerSettings{
		CheckEvery:  required(v, "check_every", "check every", 15, 10080, &errs),
		FetchCount:  num(v, "fetch_count", "how many to fetch", 1, 1000, &errs),
		Keep:        num(v, "keep", "keep in the database", 1, 10000, &errs),
		DeepDepth:   required(v, "deep_depth", "read further back depth", 1, 5000, &errs),
		Refreshes:   required(v, "refreshes", "refreshes at once", 1, 8, &errs),
		Streams:     required(v, "streams", "streams at once", 1, 16, &errs),
		HistoryDays: required(v, "history_days", "status history kept", 1, 90, &errs),
		CacheGB:     num(v, "cache_gb", "media cache size", 1, 1000, &errs),
		CacheHours:  num(v, "cache_hours", "media cache time", 1, 168, &errs),
		Quality:     quality(v, &errs),
	}
	// An app from before these settings sends none of them, which keeps the defaults.
	if s.Quality == "" {
		s.Quality = store.DefaultServerSettings().Quality
	}
	if s.CacheGB == 0 {
		s.CacheGB = store.DefaultServerSettings().CacheGB
	}
	if s.CacheHours == 0 {
		s.CacheHours = store.DefaultServerSettings().CacheHours
	}
	return s, errors.Join(errs...)
}

// PrefsIn is a prefs form; an app from before Remember speed sends none, which keeps the saved one.
type PrefsIn struct {
	Prefs       store.Prefs
	speedUnsent bool
}

// Over returns the new prefs, keeping from saved what the form did not send.
func (p PrefsIn) Over(saved store.Prefs) store.Prefs {
	out := p.Prefs
	if p.speedUnsent {
		out.RememberSpeed = saved.RememberSpeed
	}
	return out
}

func ParsePrefs(v Values) (PrefsIn, error) {
	in, err := parsePrefs(v)
	return PrefsIn{Prefs: in, speedUnsent: !v.Has("remember_speed")}, err
}

func parsePrefs(v Values) (store.Prefs, error) {
	var errs []error
	p := store.Prefs{
		TimeZone:      str(v, "time_zone"),
		SkipBack:      required(v, "skip_back", "skip back", 1, 300, &errs),
		SkipForward:   required(v, "skip_forward", "skip forward", 1, 300, &errs),
		NextAt:        num(v, "next_at", "offer Next in the last", 1, 50, &errs),
		Theme:         str(v, "theme"),
		RememberSpeed: checked(v, "remember_speed"),
	}
	if p.TimeZone == "" {
		p.TimeZone = "UTC"
	}
	if p.NextAt == 0 {
		p.NextAt = store.DefaultPrefs().NextAt
	}
	if _, err := time.LoadLocation(p.TimeZone); err != nil {
		errs = append(errs, fmt.Errorf("time zone %q is not known", p.TimeZone))
	}
	if p.Theme != "system" && p.Theme != "light" && p.Theme != "dark" {
		errs = append(errs, errors.New("theme: system, light or dark"))
	}
	return p, errors.Join(errs...)
}

type TagRename struct{ From, To string }

func ParseTagRename(v Values) (TagRename, error) {
	var errs []error
	from := store.NormalizeTags(v.Get("from"))
	to := tags(v.Get("to"), &errs)
	if len(from) != 1 || len(to) != 1 {
		errs = append(errs, errors.New("new name: one tag"))
		return TagRename{}, errors.Join(errs...)
	}
	return TagRename{From: from[0], To: to[0]}, errors.Join(errs...)
}

// Selection is several episodes and what to do with them.
type Selection struct {
	IDs    []int64
	Action string
}

var selectionActions = map[string]bool{"played": true, "unplayed": true, "star": true, "unstar": true}

func ParseSelection(ids []string, action string) (Selection, error) {
	s := Selection{Action: action}
	if !selectionActions[action] {
		return s, errors.New("unknown action")
	}
	for _, raw := range ids {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return s, errors.New("bad episode id")
		}
		s.IDs = append(s.IDs, id)
	}
	if len(s.IDs) == 0 {
		return s, errors.New("select at least one episode")
	}
	if len(s.IDs) > 5000 {
		return s, errors.New("at most 5000 episodes at once")
	}
	return s, nil
}

// EpisodeAction is one action on one episode from its page or the player.
type EpisodeAction struct {
	Action   string
	Position int
}

var episodeActions = map[string]bool{"star": true, "unstar": true, "played": true, "unplayed": true, "older-played": true, "position": true}

func ParseEpisodeAction(v Values) (EpisodeAction, error) {
	var errs []error
	a := EpisodeAction{Action: str(v, "action"), Position: num(v, "position", "position", 0, 1<<30, &errs)}
	if !episodeActions[a.Action] {
		errs = append(errs, errors.New("unknown action"))
	}
	return a, errors.Join(errs...)
}

type Move struct {
	ID     int64
	Target int64
	After  bool
}

func ParseMove(v Values) (Move, error) {
	id, err1 := strconv.ParseInt(str(v, "id"), 10, 64)
	target, err2 := strconv.ParseInt(str(v, "target"), 10, 64)
	if err1 != nil || err2 != nil || id == target {
		return Move{}, errors.New("move: give the item and where it goes")
	}
	return Move{ID: id, Target: target, After: str(v, "place") == "after"}, nil
}

// EpisodeQuery is the Episodes page's address: filter, sort and paging.
type EpisodeQuery struct {
	Filter string
	Sort   string
	Quick  string
	Page   int
}

func ParseEpisodeQuery(v Values) EpisodeQuery {
	q := EpisodeQuery{Filter: str(v, "q"), Sort: str(v, "sort"), Quick: str(v, "quick")}
	if !validSort(q.Sort) || q.Sort == "manual" || q.Sort == "shuffle" {
		q.Sort = "newest"
	}
	switch q.Quick {
	case "unplayed":
		q.Filter = "$unplayed"
	case "inprogress":
		q.Filter = "$inprogress"
	case "all":
		q.Filter = ""
	}
	if n, err := strconv.Atoi(str(v, "page")); err == nil && n > 0 && n < 100000 {
		q.Page = n
	}
	return q
}

// Client is the address wrong passwords are counted against: the peer, or whoever a proxy on a private network says it forwards.
func Client(remoteAddr string, forwardedFor []string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if !feed.Public(ip) && len(forwardedFor) > 0 {
		// Only the last entry was written by that proxy; the ones before it are the client's own claim.
		hops := strings.Split(forwardedFor[len(forwardedFor)-1], ",")
		if fwd := net.ParseIP(strings.TrimSpace(hops[len(hops)-1])); fwd != nil {
			ip = fwd
		}
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String()
}

type SignIn struct {
	Password string
	Device   string
}

func ParseSignIn(v Values) (SignIn, error) {
	s := SignIn{Password: v.Get("password"), Device: str(v, "device")}
	if s.Password == "" {
		return s, errors.New("password is required")
	}
	if s.Device == "" {
		s.Device = "App"
	}
	if utf8.RuneCountInString(s.Device) > 80 {
		return s, errors.New("device name: at most 80 characters")
	}
	return s, nil
}

// PlayChanges validates the changes a device pushes when it syncs.
type PlayChangeIn struct {
	Episode  int64  `json:"episode"`
	Field    string `json:"field"`
	Position int    `json:"position"`
	Value    bool   `json:"value"`
	At       int64  `json:"at"`
}

func ParsePlayChanges(in []PlayChangeIn) ([]store.PlayChange, error) {
	if len(in) > 20000 {
		return nil, errors.New("too many changes at once")
	}
	out := make([]store.PlayChange, 0, len(in))
	maxAt := time.Now().Add(5 * time.Minute).UnixMilli()
	for i, c := range in {
		switch c.Field {
		case store.FieldPosition, store.FieldPlayed, store.FieldStarred:
		default:
			return nil, fmt.Errorf("change %d: unknown field %q", i, c.Field)
		}
		if c.Episode <= 0 || c.At <= 0 || c.Position < 0 {
			return nil, fmt.Errorf("change %d: episode, time and position must be positive", i)
		}
		out = append(out, store.PlayChange{EpisodeID: c.Episode, Field: c.Field, Position: c.Position, Value: c.Value, At: min(c.At, maxAt)})
	}
	return out, nil
}
