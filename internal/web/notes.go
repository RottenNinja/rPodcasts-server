package web

import (
	"html"
	"html/template"
	"regexp"
	"strconv"
	"strings"
)

var (
	tagRe       = regexp.MustCompile(`(?s)<(/?)([a-zA-Z][a-zA-Z0-9]*)([^>]*)>`)
	hrefRe      = regexp.MustCompile(`(?i)\bhref\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`)
	dropRe      = regexp.MustCompile(`(?is)<(script|style|iframe|object|embed|noscript|template)\b.*?</(script|style|iframe|object|embed|noscript|template)\s*>|<!--.*?-->`)
	timestampRe = regexp.MustCompile(`\b(?:(\d{1,2}):)?([0-5]?\d):([0-5]\d)\b`)
	urlRe       = regexp.MustCompile(`https?://[^\s<>"']+[^\s<>"'.,;:!?)\]]`)
	allowed     = map[string]string{"p": "p", "br": "br", "b": "strong", "strong": "strong", "i": "em", "em": "em", "u": "u", "ul": "ul", "ol": "ol",
		"li": "li", "blockquote": "blockquote", "h1": "h3", "h2": "h3", "h3": "h3", "h4": "h4", "h5": "h4", "h6": "h4", "a": "a", "code": "code", "pre": "pre", "hr": "hr"}
)

// Sanitize keeps a small set of tags from show notes, escapes the rest, and makes timestamps seek the player.
func Sanitize(notes string) template.HTML {
	src := dropRe.ReplaceAllString(notes, "")
	if !strings.Contains(src, "<") {
		src = strings.ReplaceAll(html.EscapeString(html.UnescapeString(src)), "\n", "<br>")
		return template.HTML(linkText(src, true))
	}
	var b strings.Builder
	open := map[string]int{}
	last := 0
	inLink := 0
	for _, m := range tagRe.FindAllStringSubmatchIndex(src, -1) {
		b.WriteString(linkText(html.EscapeString(html.UnescapeString(src[last:m[0]])), inLink == 0))
		last = m[1]
		closing := src[m[2]:m[3]] == "/"
		name := strings.ToLower(src[m[4]:m[5]])
		attrs := src[m[6]:m[7]]
		out, ok := allowed[name]
		if !ok {
			if name == "div" || name == "section" || name == "article" {
				b.WriteString(" ")
			}
			continue
		}
		void := out == "br" || out == "hr"
		if closing {
			if void || open[out] == 0 {
				continue
			}
			open[out]--
			if out == "a" {
				inLink--
			}
			b.WriteString("</" + out + ">")
			continue
		}
		if void {
			b.WriteString("<" + out + ">")
			continue
		}
		if out == "a" {
			href := ""
			if hm := hrefRe.FindStringSubmatch(attrs); hm != nil {
				href = strings.TrimSpace(html.UnescapeString(hm[2] + hm[3] + hm[4]))
			}
			low := strings.ToLower(href)
			if !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "https://") && !strings.HasPrefix(low, "mailto:") {
				continue
			}
			b.WriteString(`<a href="` + html.EscapeString(href) + `" target="_blank" rel="noopener noreferrer nofollow">`)
			inLink++
		} else {
			b.WriteString("<" + out + ">")
		}
		open[out]++
	}
	b.WriteString(linkText(html.EscapeString(html.UnescapeString(src[last:])), inLink == 0))
	for t, n := range open {
		for range n {
			b.WriteString("</" + t + ">")
		}
	}
	return template.HTML(b.String())
}

// linkText turns timestamps into seek links and bare addresses into links, in already-escaped text.
func linkText(escaped string, links bool) string {
	if !links {
		return escaped
	}
	out := timestampRe.ReplaceAllStringFunc(escaped, func(m string) string {
		p := strings.Split(m, ":")
		secs := 0
		for _, x := range p {
			n, _ := strconv.Atoi(x)
			secs = secs*60 + n
		}
		return `<a href="#" class="ts" data-seek="` + strconv.Itoa(secs) + `">` + m + `</a>`
	})
	return urlRe.ReplaceAllStringFunc(out, func(u string) string {
		if strings.Contains(u, "data-seek") {
			return u
		}
		return `<a href="` + u + `" target="_blank" rel="noopener noreferrer nofollow">` + u + `</a>`
	})
}
