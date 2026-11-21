package feed

import (
	"encoding/xml"
	"io"
	"strings"
	"time"
)

type OPMLEntry struct {
	URL    string
	Title  string
	Folder string
}

type outline struct {
	Text     string    `xml:"text,attr"`
	Title    string    `xml:"title,attr,omitempty"`
	Type     string    `xml:"type,attr,omitempty"`
	XMLURL   string    `xml:"xmlUrl,attr,omitempty"`
	HTMLURL  string    `xml:"htmlUrl,attr,omitempty"`
	Outlines []outline `xml:"outline"`
}

// ParseOPML lists every feed in the file with the folder it sits in.
func ParseOPML(r io.Reader) ([]OPMLEntry, error) {
	var doc struct {
		Body struct {
			Outlines []outline `xml:"outline"`
		} `xml:"body"`
	}
	d := xml.NewDecoder(io.LimitReader(r, 16<<20))
	d.Strict = false
	d.CharsetReader = func(label string, input io.Reader) (io.Reader, error) {
		if t := singleByte(label); t != nil {
			return &decoder{src: input, table: t}, nil
		}
		return input, nil
	}
	if err := d.Decode(&doc); err != nil {
		return nil, err
	}
	var out []OPMLEntry
	var walk func([]outline, string)
	walk = func(list []outline, folder string) {
		for _, o := range list {
			if o.XMLURL != "" {
				out = append(out, OPMLEntry{URL: strings.TrimSpace(o.XMLURL), Title: firstNonEmpty(o.Title, o.Text), Folder: folder})
			}
			if len(o.Outlines) > 0 {
				f := folder
				if o.XMLURL == "" {
					f = firstNonEmpty(o.Title, o.Text)
				}
				walk(o.Outlines, f)
			}
		}
	}
	walk(doc.Body.Outlines, "")
	return out, nil
}

// WriteOPML writes feeds as one flat list.
func WriteOPML(w io.Writer, entries []OPMLEntry) error {
	type head struct {
		Title       string `xml:"title"`
		DateCreated string `xml:"dateCreated"`
	}
	type doc struct {
		XMLName xml.Name `xml:"opml"`
		Version string   `xml:"version,attr"`
		Head    head     `xml:"head"`
		Body    struct {
			Outlines []outline `xml:"outline"`
		} `xml:"body"`
	}
	d := doc{Version: "2.0", Head: head{Title: "Podcasts", DateCreated: time.Now().UTC().Format(time.RFC1123Z)}}
	for _, e := range entries {
		d.Body.Outlines = append(d.Body.Outlines, outline{Text: e.Title, Title: e.Title, Type: "rss", XMLURL: e.URL})
	}
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	return enc.Encode(d)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
