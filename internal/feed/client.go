// Package feed fetches addresses safely and reads RSS and Atom feeds as a stream.
package feed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var ErrPrivate = errors.New("address resolves to a private or loopback network")

// Client refuses to connect to private, loopback and link-local addresses, unless its Guard allows the local network.
type Client struct {
	HTTP  *http.Client
	Guard Guard
}

func NewClient(g Guard) *Client {
	dialer := &net.Dialer{
		Timeout: 20 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || !g.ip(ip) {
				return ErrPrivate
			}
			return nil
		},
	}
	tr := &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          50,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	return &Client{HTTP: &http.Client{Transport: tr}, Guard: g}
}

// Guard decides which networks the server may reach; its zero value allows public ones only.
type Guard struct {
	// Local also allows private, loopback and link-local networks.
	Local bool
}

func (g Guard) ip(ip net.IP) bool {
	return g.Local || Public(ip)
}

// Web refuses anything but a web address whose host resolves only to networks the guard allows.
func (g Guard) Web(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !IsWeb(raw) || u.Hostname() == "" {
		return errors.New("not a web address")
	}
	if g.Local {
		return nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
	if err != nil {
		return err
	}
	for _, ip := range ips {
		if !Public(ip.IP) {
			return ErrPrivate
		}
	}
	return nil
}

// IsWeb reports whether an address is http or https; any other scheme is only for the downloader engine.
func IsWeb(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https")
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func Public(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() || cgnat.Contains(ip))
}

const userAgent = "Mozilla/5.0 (compatible; podcasts/1.0; +self-hosted)"

type Request struct {
	URL          string
	ETag         string
	LastModified string
	Username     string
	Password     string
	Limit        int
}

type Result struct {
	NotModified  bool
	URL          string
	ETag         string
	LastModified string
	Feed         *Feed
	FeedLinks    []string
}

// Fetch reads an address: a feed is parsed, a web page yields the feeds it links to.
func (c *Client) Fetch(ctx context.Context, r Request) (Result, error) {
	permanent := true
	finalURL := r.URL
	cl := *c.HTTP
	cl.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if req.Response != nil && req.Response.StatusCode != http.StatusMovedPermanently && req.Response.StatusCode != http.StatusPermanentRedirect {
			permanent = false
		}
		if permanent {
			finalURL = req.URL.String()
		}
		if r.Username != "" && req.URL.Host == via[0].URL.Host {
			req.SetBasicAuth(r.Username, r.Password)
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml;q=0.9, text/xml;q=0.9, text/html;q=0.8, */*;q=0.5")
	if r.ETag != "" {
		req.Header.Set("If-None-Match", r.ETag)
	}
	if r.LastModified != "" {
		req.Header.Set("If-Modified-Since", r.LastModified)
	}
	if r.Username != "" {
		req.SetBasicAuth(r.Username, r.Password)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	res := Result{URL: finalURL, ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified")}
	switch {
	case resp.StatusCode == http.StatusNotModified:
		res.NotModified = true
		return res, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return res, errors.New("the feed needs a user name and password (401)")
	case resp.StatusCode >= 400:
		return res, fmt.Errorf("server answered %s", resp.Status)
	}
	body := io.LimitReader(resp.Body, 64<<20)
	peek := make([]byte, 1024)
	n, _ := io.ReadFull(body, peek)
	peek = peek[:n]
	full := io.MultiReader(strings.NewReader(string(peek)), body)
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	head := strings.ToLower(string(peek))
	if strings.Contains(ct, "html") || strings.Contains(head, "<!doctype html") || strings.Contains(head, "<html") {
		b, _ := io.ReadAll(io.LimitReader(full, 4<<20))
		res.FeedLinks = feedLinks(string(b), resp.Request.URL)
		return res, nil
	}
	f, err := Parse(full, charsetOf(ct), r.Limit)
	if err != nil {
		return res, err
	}
	res.Feed = f
	return res, nil
}

func charsetOf(ct string) string {
	_, cs, ok := strings.Cut(ct, "charset=")
	if !ok {
		return ""
	}
	return strings.Trim(strings.TrimSpace(strings.Split(cs, ";")[0]), `"'`)
}

var (
	linkTagRe = regexp.MustCompile(`(?is)<link\b[^>]*>`)
	attrRe    = regexp.MustCompile(`(?is)([a-z-]+)\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
)

// feedLinks finds the feeds a web page announces with <link rel="alternate">.
func feedLinks(html string, base *url.URL) []string {
	var out []string
	for _, tag := range linkTagRe.FindAllString(html, -1) {
		attrs := map[string]string{}
		for _, m := range attrRe.FindAllStringSubmatch(tag, -1) {
			attrs[strings.ToLower(m[1])] = strings.Trim(m[2], `"'`)
		}
		t := strings.ToLower(attrs["type"])
		if !strings.Contains(strings.ToLower(attrs["rel"]), "alternate") || (!strings.Contains(t, "rss") && !strings.Contains(t, "atom")) {
			continue
		}
		u, err := base.Parse(strings.ReplaceAll(strings.TrimSpace(attrs["href"]), "&amp;", "&"))
		if err == nil && (u.Scheme == "http" || u.Scheme == "https") {
			out = append(out, u.String())
		}
	}
	return out
}
