package feed

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Feed struct {
	Title       string
	Author      string
	Website     string
	Description string
	Artwork     string
	NewFeedURL  string
	Items       []Item
	Total       int
}

type Item struct {
	GUID          string
	Title         string
	Link          string
	Notes         string
	Published     time.Time
	MediaURL      string
	MIME          string
	Size          int64
	Media         string
	Duration      int
	Season        int
	Number        int
	Type          string
	Artwork       string
	ChaptersURL   string
	TranscriptURL string
}

// elem is a small subtree: one channel field or one whole item.
type elem struct {
	ns, name string
	attrs    map[string]string
	text     strings.Builder
	kids     []*elem
}

func (e *elem) attr(k string) string { return e.attrs[k] }

func nsOf(space string) string {
	s := strings.ToLower(space)
	switch {
	case strings.Contains(s, "itunes.com"):
		return "itunes"
	case strings.Contains(s, "podcastindex"):
		return "podcast"
	case strings.Contains(s, "rss/1.0/modules/content"):
		return "content"
	case strings.Contains(s, "search.yahoo.com/mrss"):
		return "media"
	case strings.Contains(s, "purl.org/dc"):
		return "dc"
	case strings.Contains(s, "googleplay"):
		return "googleplay"
	}
	return ""
}

func readElem(d *xml.Decoder, start xml.StartElement, depth int) (*elem, error) {
	e := &elem{ns: nsOf(start.Name.Space), name: strings.ToLower(start.Name.Local), attrs: map[string]string{}}
	for _, a := range start.Attr {
		e.attrs[strings.ToLower(a.Name.Local)] = strings.TrimSpace(a.Value)
	}
	for {
		tok, err := d.Token()
		if err != nil {
			return e, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth > 12 {
				if err := d.Skip(); err != nil {
					return e, err
				}
				continue
			}
			k, err := readElem(d, t, depth+1)
			if err != nil {
				return e, err
			}
			e.kids = append(e.kids, k)
		case xml.CharData:
			if e.text.Len() < 1<<20 {
				e.text.Write(t)
			}
		case xml.EndElement:
			return e, nil
		}
	}
}

// Parse reads a feed as a stream, keeping at most limit of the newest items (400 when limit is 0).
func Parse(r io.Reader, charset string, limit int) (*Feed, error) {
	if limit <= 0 {
		limit = 400
	}
	d := xml.NewDecoder(r)
	d.Strict = false
	d.Entity = xml.HTMLEntity
	if single := singleByte(charset); single != nil {
		r = &decoder{src: r, table: single}
		d = xml.NewDecoder(r)
		d.Strict, d.Entity = false, xml.HTMLEntity
		d.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }
	} else {
		d.CharsetReader = func(label string, input io.Reader) (io.Reader, error) {
			if t := singleByte(label); t != nil {
				return &decoder{src: input, table: t}, nil
			}
			if l := strings.ToLower(label); l == "utf-8" || l == "utf8" || l == "us-ascii" || l == "ascii" {
				return input, nil
			}
			return nil, fmt.Errorf("unsupported character set %q", label)
		}
	}
	f := &Feed{}
	seenRoot := false
	trim := func() {
		sort.SliceStable(f.Items, func(i, j int) bool { return f.Items[i].Published.After(f.Items[j].Published) })
		if len(f.Items) > limit {
			f.Items = f.Items[:limit]
		}
	}
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if seenRoot {
				break
			}
			return nil, fmt.Errorf("not a feed: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		name := strings.ToLower(se.Name.Local)
		switch name {
		case "rss", "channel", "feed", "rdf":
			seenRoot = true
			continue
		}
		if !seenRoot {
			return nil, errors.New("not an RSS or Atom feed")
		}
		e, err := readElem(d, se, 0)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if name == "item" || name == "entry" {
			f.Total++
			f.Items = append(f.Items, toItem(e))
			if len(f.Items) >= 2*limit {
				trim()
			}
			continue
		}
		channelField(f, e)
	}
	if !seenRoot {
		return nil, errors.New("not an RSS or Atom feed")
	}
	trim()
	return f, nil
}

func text(e *elem) string { return strings.TrimSpace(e.text.String()) }

func channelField(f *Feed, e *elem) {
	switch {
	case e.ns == "" && e.name == "title":
		f.Title = text(e)
	case e.ns == "" && e.name == "link":
		if h := e.attr("href"); h != "" {
			if r := e.attr("rel"); r == "" || r == "alternate" {
				f.Website = h
			}
		} else if t := text(e); t != "" {
			f.Website = t
		}
	case e.ns == "" && (e.name == "description" || e.name == "subtitle"):
		f.Description = text(e)
	case e.ns == "itunes" && e.name == "summary" && f.Description == "":
		f.Description = text(e)
	case e.ns == "itunes" && e.name == "author", e.ns == "" && e.name == "managingeditor" && f.Author == "":
		f.Author = text(e)
	case e.ns == "" && e.name == "author":
		for _, k := range e.kids {
			if k.name == "name" {
				f.Author = text(k)
			}
		}
	case e.ns == "itunes" && e.name == "image":
		f.Artwork = e.attr("href")
	case e.ns == "" && e.name == "image" && f.Artwork == "":
		for _, k := range e.kids {
			if k.name == "url" {
				f.Artwork = text(k)
			}
		}
	case e.ns == "" && (e.name == "logo" || e.name == "icon") && f.Artwork == "":
		f.Artwork = text(e)
	case e.ns == "itunes" && e.name == "new-feed-url":
		f.NewFeedURL = text(e)
	}
}

func toItem(e *elem) Item {
	var it Item
	var desc, content, summary string
	for _, k := range e.kids {
		switch {
		case k.ns == "" && k.name == "title":
			it.Title = text(k)
		case k.ns == "" && (k.name == "guid" || k.name == "id"):
			it.GUID = text(k)
		case k.ns == "" && k.name == "link":
			if h := k.attr("href"); h != "" {
				switch k.attr("rel") {
				case "enclosure":
					if it.MediaURL == "" {
						it.MediaURL, it.MIME = h, k.attr("type")
						it.Size, _ = strconv.ParseInt(k.attr("length"), 10, 64)
					}
				case "", "alternate":
					it.Link = h
				}
			} else if t := text(k); t != "" {
				it.Link = t
			}
		case k.ns == "" && k.name == "enclosure":
			if it.MediaURL == "" || !playable(it.MIME, it.MediaURL) {
				it.MediaURL, it.MIME = k.attr("url"), k.attr("type")
				it.Size, _ = strconv.ParseInt(k.attr("length"), 10, 64)
			}
		case k.ns == "media" && k.name == "content" && it.MediaURL == "":
			it.MediaURL, it.MIME = k.attr("url"), k.attr("type")
			if it.Duration == 0 {
				it.Duration = ParseDuration(k.attr("duration"))
			}
		case k.ns == "media" && k.name == "group":
			for _, g := range k.kids {
				if g.name == "content" && it.MediaURL == "" {
					it.MediaURL, it.MIME = g.attr("url"), g.attr("type")
				}
				if g.name == "thumbnail" && it.Artwork == "" {
					it.Artwork = g.attr("url")
				}
				if g.name == "description" && desc == "" {
					desc = text(g)
				}
			}
		case k.ns == "" && k.name == "description":
			desc = text(k)
		case k.ns == "content" && k.name == "encoded":
			content = text(k)
		case k.ns == "" && k.name == "content":
			content = text(k)
		case k.name == "summary":
			if summary == "" {
				summary = text(k)
			}
		case k.ns == "" && k.name == "pubdate", k.ns == "" && k.name == "published", k.ns == "dc" && k.name == "date":
			if t, ok := ParseDate(text(k)); ok {
				it.Published = t
			}
		case k.ns == "" && k.name == "updated":
			if t, ok := ParseDate(text(k)); ok && it.Published.IsZero() {
				it.Published = t
			}
		case k.ns == "itunes" && k.name == "duration":
			it.Duration = ParseDuration(text(k))
		case k.ns == "itunes" && k.name == "season":
			it.Season, _ = strconv.Atoi(text(k))
		case k.ns == "itunes" && k.name == "episode":
			it.Number, _ = strconv.Atoi(text(k))
		case k.ns == "itunes" && k.name == "episodetype":
			it.Type = strings.ToLower(text(k))
		case k.ns == "itunes" && k.name == "image":
			it.Artwork = k.attr("href")
		case k.ns == "podcast" && k.name == "chapters":
			it.ChaptersURL = k.attr("url")
		case k.ns == "podcast" && k.name == "transcript":
			if it.TranscriptURL == "" || strings.Contains(k.attr("type"), "html") || strings.Contains(k.attr("type"), "text/plain") {
				it.TranscriptURL = k.attr("url")
			}
		case k.ns == "podcast" && k.name == "season" && it.Season == 0:
			it.Season, _ = strconv.Atoi(text(k))
		case k.ns == "podcast" && k.name == "episode" && it.Number == 0:
			n, _ := strconv.ParseFloat(text(k), 64)
			it.Number = int(n)
		}
	}
	switch {
	case content != "":
		it.Notes = content
	case desc != "":
		it.Notes = desc
	default:
		it.Notes = summary
	}
	if it.Type != "trailer" && it.Type != "bonus" {
		it.Type = "full"
	}
	if it.Season < 0 {
		it.Season = 0
	}
	if it.Number < 0 {
		it.Number = 0
	}
	if it.MediaURL != "" && !playable(it.MIME, it.MediaURL) {
		it.MediaURL, it.MIME = "", ""
	}
	if it.MediaURL != "" {
		it.Media = MediaKind(it.MIME, it.MediaURL)
	}
	if it.GUID == "" {
		it.GUID = it.MediaURL
	}
	if it.GUID == "" {
		it.GUID = it.Link
	}
	if it.Title == "" {
		it.Title = "Untitled"
	}
	return it
}

var (
	audioExt = map[string]bool{".mp3": true, ".m4a": true, ".aac": true, ".ogg": true, ".oga": true, ".opus": true, ".wav": true, ".flac": true, ".m4b": true}
	videoExt = map[string]bool{".mp4": true, ".m4v": true, ".mov": true, ".webm": true, ".mkv": true, ".avi": true}
)

func extOf(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	return strings.ToLower(path.Ext(u))
}

func playable(mime, u string) bool {
	m := strings.ToLower(mime)
	if strings.HasPrefix(m, "audio/") || strings.HasPrefix(m, "video/") {
		return true
	}
	e := extOf(u)
	return audioExt[e] || videoExt[e] || (m == "" && e == "")
}

// MediaKind works out audio or video from the MIME type, or else from the file extension.
func MediaKind(mime, u string) string {
	m := strings.ToLower(mime)
	switch {
	case strings.HasPrefix(m, "video/"):
		return "video"
	case strings.HasPrefix(m, "audio/"):
		return "audio"
	}
	if videoExt[extOf(u)] {
		return "video"
	}
	return "audio"
}

// ParseDuration reads seconds, MM:SS or HH:MM:SS.
func ParseDuration(v string) int {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	total := 0.0
	for _, p := range strings.Split(v, ":") {
		n, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || n < 0 {
			return 0
		}
		total = total*60 + n
	}
	return int(total)
}

var (
	spaceRe   = regexp.MustCompile(`\s+`)
	weekdayRe = regexp.MustCompile(`^[A-Za-z]+,?\s+`)
	zoneAbbr  = map[string]string{
		"GMT": "+0000", "UT": "+0000", "UTC": "+0000", "Z": "+0000", "EST": "-0500", "EDT": "-0400", "CST": "-0600", "CDT": "-0500",
		"MST": "-0700", "MDT": "-0600", "PST": "-0800", "PDT": "-0700", "BST": "+0100", "CET": "+0100", "CEST": "+0200", "EET": "+0200",
		"EEST": "+0300", "WET": "+0000", "WEST": "+0100", "AEST": "+1000", "AEDT": "+1100", "IST": "+0530", "JST": "+0900", "KST": "+0900",
		"HST": "-1000", "AKST": "-0900", "AKDT": "-0800", "NZST": "+1200", "NZDT": "+1300", "MSK": "+0300",
	}
	rfc822Layouts = []string{
		"2 Jan 2006 15:04:05 -0700", "2 Jan 2006 15:04 -0700", "2 Jan 06 15:04:05 -0700", "2 Jan 06 15:04 -0700",
		"2 January 2006 15:04:05 -0700", "2 January 2006 15:04 -0700", "2 Jan 2006 15:04:05", "2 Jan 2006 15:04", "2 Jan 2006",
		"2 January 2006", "Jan 2 2006 15:04:05 -0700", "Jan 2, 2006 15:04:05 -0700", "January 2, 2006", "Jan 2, 2006",
	}
	isoLayouts = []string{time.RFC3339Nano, "2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05", "2006-01-02T15:04Z07:00", "2006-01-02T15:04",
		"2006-01-02 15:04:05 -0700", "2006-01-02 15:04:05Z07:00", "2006-01-02 15:04:05", "2006-01-02"}
)

// ParseDate accepts every RFC 822 variant seen in feeds and RFC 3339.
func ParseDate(v string) (time.Time, bool) {
	v = strings.TrimSpace(spaceRe.ReplaceAllString(v, " "))
	if v == "" {
		return time.Time{}, false
	}
	for _, l := range isoLayouts {
		if t, err := time.Parse(l, v); err == nil {
			return t, true
		}
	}
	s := weekdayRe.ReplaceAllString(v, "")
	s = strings.ReplaceAll(s, ",", "")
	fields := strings.Fields(s)
	if n := len(fields); n > 0 {
		last := strings.ToUpper(strings.Trim(fields[n-1], "()"))
		if off, ok := zoneAbbr[last]; ok {
			fields[n-1] = off
		} else if len(last) >= 3 && strings.IndexFunc(last, func(r rune) bool { return r < 'A' || r > 'Z' }) < 0 && n >= 4 {
			fields[n-1] = "+0000"
		} else if strings.HasPrefix(last, "GMT") || strings.HasPrefix(last, "UTC") {
			fields[n-1] = strings.ReplaceAll(last[3:], ":", "")
		} else if (strings.HasPrefix(last, "+") || strings.HasPrefix(last, "-")) && strings.Contains(last, ":") {
			fields[n-1] = strings.ReplaceAll(last, ":", "")
		}
		if n >= 5 && len(fields[n-1]) == 5 && (fields[n-1][0] == '+' || fields[n-1][0] == '-') && len(fields[n-2]) == 5 && strings.Count(fields[n-2], ":") == 1 {
			fields[n-2] += ":00"
		}
	}
	s = strings.Join(fields, " ")
	for _, l := range append(rfc822Layouts, strings.Replace(rfc822Layouts[0], "Jan", "January", 1)) {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	if !strings.ContainsRune(s, ',') {
		for _, l := range []string{"2 Jan 2006 15:04:05 -0700"} {
			if t, err := time.Parse(l, fixMonth(s)); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// fixMonth trims long or dotted month names like "Sept." to three letters.
func fixMonth(s string) string {
	f := strings.Fields(s)
	if len(f) > 1 && len(f[1]) > 3 && utf8.ValidString(f[1]) {
		f[1] = strings.TrimSuffix(f[1], ".")
		if len(f[1]) > 3 {
			f[1] = f[1][:3]
		}
	}
	return strings.Join(f, " ")
}
