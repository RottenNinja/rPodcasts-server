package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"podcasts/internal/feed"
)

// Command is an engine run as an external command-line program that prints JSON.
type Command struct {
	bin, url string
	args     Args
	guard    feed.Guard
}

type CommandOptions struct {
	// URL is where to download the engine to bin when it isn't there; empty means it is installed by hand.
	URL   string
	Guard feed.Guard
}

func NewCommand(bin string, args Args, o CommandOptions) *Command {
	return &Command{bin: bin, url: o.URL, args: args, guard: o.Guard}
}

// Args are the arguments the engine is run with at each touchpoint, one word per argument, with placeholders in braces.
type Args struct {
	List    []string
	Artwork []string
	Video   []string
	Resolve []string
	Version []string
	Update  []string
}

// DefaultArgs name the touchpoint first, for a script of one's own to switch on.
func DefaultArgs() Args {
	return Args{
		List:    []string{"list", "{url}", "{limit}"},
		Artwork: []string{"artwork", "{url}"},
		Video:   []string{"video", "{url}"},
		Resolve: []string{"resolve", "{url}", "{quality}"},
		Version: []string{"version"},
		Update:  []string{"update"},
	}
}

var placeholder = regexp.MustCompile(`\{[^{}]*\}`)

// Check refuses a placeholder a touchpoint doesn't fill, and a touchpoint that reads an address without {url}.
func (a Args) Check() error {
	var errs []error
	for _, t := range []struct {
		name     string
		args     []string
		known    string
		needsURL bool
	}{
		{"LIST", a.List, "{url} {limit}", true},
		{"ARTWORK", a.Artwork, "{url}", len(a.Artwork) > 0},
		{"VIDEO", a.Video, "{url}", true},
		{"RESOLVE", a.Resolve, "{url} {quality} {format}", true},
		{"VERSION", a.Version, "", false},
		{"UPDATE", a.Update, "", false},
	} {
		hasURL := false
		for _, w := range t.args {
			for _, p := range placeholder.FindAllString(w, -1) {
				hasURL = hasURL || p == "{url}"
				if !slices.Contains(strings.Fields(t.known), p) {
					errs = append(errs, fmt.Errorf("PODCASTS_ENGINE_ARGS_%s: %s is not filled in here", t.name, p))
				}
			}
		}
		if t.needsURL && !hasURL {
			errs = append(errs, fmt.Errorf("PODCASTS_ENGINE_ARGS_%s: needs {url}", t.name))
		}
	}
	return errors.Join(errs...)
}

// cmd runs the engine with a touchpoint's arguments, its placeholders filled in from vals.
func (r *Command) cmd(ctx context.Context, args []string, vals map[string]string) *exec.Cmd {
	filled := make([]string, len(args))
	for i, w := range args {
		filled[i] = placeholder.ReplaceAllStringFunc(w, func(p string) string { return vals[p] })
	}
	return exec.CommandContext(ctx, r.bin, filled...)
}

// allowed passes any address that isn't a web one, which only the engine can read, and checks a web one against the guard.
func (r *Command) allowed(ctx context.Context, url string) error {
	if !feed.IsWeb(url) {
		return nil
	}
	return r.guard.Web(ctx, url)
}

func (r *Command) Missing() bool {
	_, err := exec.LookPath(r.bin)
	return r.url != "" && err != nil
}

// Install downloads the engine to its path and makes it executable.
func (r *Command) Install(ctx context.Context) error {
	if r.url == "" {
		return errors.New("the downloader engine is missing and PODCASTS_ENGINE_URL is not set")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if ue := (*url.Error)(nil); errors.As(err, &ue) {
		return fmt.Errorf("download: %w", ue.Err)
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: %s", resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(r.bin), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.bin), ".engine-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, io.LimitReader(resp.Body, 1<<30)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), r.bin)
}

var (
	siteTag = regexp.MustCompile(`^(ERROR: )?(\[[^\]]*\] )?`)
	link    = regexp.MustCompile(`\s*\(?https?://\S+`)
)

// errText keeps the last meaningful line the engine wrote, without its site tag or links.
func errText(stderr []byte, err error) error {
	lines := strings.Split(strings.TrimSpace(string(stderr)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return errors.New(link.ReplaceAllString(siteTag.ReplaceAllString(l, ""), ""))
		}
	}
	return err
}

type flat struct {
	ID               string  `json:"id"`
	URL              string  `json:"url"`
	WebpageURL       string  `json:"webpage_url"`
	Title            string  `json:"title"`
	Duration         float64 `json:"duration"`
	Timestamp        float64 `json:"timestamp"`
	ReleaseTimestamp float64 `json:"release_timestamp"`
	UploadDate       string  `json:"upload_date"`
	Thumbnails       []struct {
		URL string `json:"url"`
	} `json:"thumbnails"`
	Thumbnail        string `json:"thumbnail"`
	Type             string `json:"_type"`
	LiveStatus       string `json:"live_status"`
	Availability     string `json:"availability"`
	PlaylistTitle    string `json:"playlist_title"`
	PlaylistUploader string `json:"playlist_uploader"`
	PlaylistChannel  string `json:"playlist_channel"`
	PlaylistURL      string `json:"playlist_webpage_url"`
	PlaylistCount    int    `json:"playlist_count"`
	Uploader         string `json:"uploader"`
	Channel          string `json:"channel"`
	Description      string `json:"description"`
	Chapters         []struct {
		Start float64 `json:"start_time"`
		Title string  `json:"title"`
	} `json:"chapters"`
	stream
	Formats []stream `json:"formats"`
}

// stream is what the engine says one format holds; a codec is "none" when absent and empty when unknown.
type stream struct {
	Ext    string  `json:"ext"`
	VCodec string  `json:"vcodec"`
	ACodec string  `json:"acodec"`
	Height float64 `json:"height"`
}

var audioExts = map[string]bool{"mp3": true, "m4a": true, "aac": true, "ogg": true, "oga": true, "opus": true, "flac": true, "wav": true, "wma": true, "mka": true, "weba": true}

// media is "video" when any format has a picture, "audio" when only sound was found, and empty when neither is known.
func (f flat) media() string {
	all := f.Formats
	if len(all) == 0 {
		all = []stream{f.stream}
	}
	audio := false
	for _, s := range all {
		switch {
		case s.VCodec == "none":
			audio = audio || s.ACodec != "none"
		case s.VCodec != "" || s.Height > 0:
			return "video"
		case audioExts[s.Ext]:
			audio = true
		}
	}
	if audio {
		return "audio"
	}
	return ""
}

func (f flat) video() Video {
	v := Video{Title: f.Title, Published: f.when(), Duration: int(f.Duration), Description: f.Description, Thumbnail: f.Thumbnail, Media: f.media()}
	for _, c := range f.Chapters {
		v.Chapters = append(v.Chapters, Chapter{Start: int(c.Start), Title: c.Title})
	}
	return v
}

func (f flat) when() time.Time {
	switch {
	case f.Timestamp > 0:
		return time.Unix(int64(f.Timestamp), 0)
	case f.ReleaseTimestamp > 0:
		return time.Unix(int64(f.ReleaseTimestamp), 0)
	}
	if t, err := time.Parse("20060102", f.UploadDate); err == nil {
		return t
	}
	return time.Time{}
}

// List reads a page of videos, somewhat past limit so the caller can cut to it; progress is called after each one.
func (r *Command) List(ctx context.Context, url string, limit int, progress func(n int)) (ListInfo, error) {
	if err := r.allowed(ctx, url); err != nil {
		return ListInfo{}, err
	}
	// Members-only videos count towards the end but are dropped, so ask for some more; the caller cuts to its limit.
	c := r.cmd(ctx, r.args.List, map[string]string{"{url}": url, "{limit}": fmt.Sprint(limit + min(limit, 50))})
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.StdoutPipe()
	if err != nil {
		return ListInfo{}, err
	}
	if err := c.Start(); err != nil {
		return ListInfo{}, err
	}
	var info ListInfo
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var f flat
		if json.Unmarshal(sc.Bytes(), &f) != nil {
			continue
		}
		if info.Title == "" {
			info.Title = first(f.PlaylistTitle, f.PlaylistChannel, f.Channel)
			info.Author = first(f.PlaylistUploader, f.PlaylistChannel, f.Uploader, f.Channel)
			info.Website = f.PlaylistURL
			info.Count = f.PlaylistCount
		}
		if f.LiveStatus == "is_upcoming" || f.LiveStatus == "is_live" {
			continue
		}
		if f.Availability == "subscriber_only" || f.Availability == "premium_only" {
			info.Locked = append(info.Locked, first(f.ID, f.WebpageURL, f.URL))
			continue
		}
		e := Entry{ID: f.ID, URL: first(f.WebpageURL, f.URL), Title: f.Title, Duration: int(f.Duration), Timestamp: f.when(), Thumbnail: f.Thumbnail}
		if e.Thumbnail == "" && len(f.Thumbnails) > 0 {
			e.Thumbnail = f.Thumbnails[len(f.Thumbnails)-1].URL
		}
		if e.URL == "" {
			continue
		}
		info.Entries = append(info.Entries, e)
		if progress != nil {
			progress(len(info.Entries))
		}
	}
	werr := c.Wait()
	if ctx.Err() != nil {
		return info, ctx.Err()
	}
	if werr != nil && len(info.Entries) == 0 && len(info.Locked) == 0 {
		return info, errText(stderr.Bytes(), werr)
	}
	info.Artwork = r.pageArtwork(ctx, url)
	return info, nil
}

// pageArtwork asks for the page's own picture, such as a channel's avatar; the entries carry only their own thumbnails.
func (r *Command) pageArtwork(ctx context.Context, url string) string {
	if len(r.args.Artwork) == 0 {
		return ""
	}
	out, err := r.cmd(ctx, r.args.Artwork, map[string]string{"{url}": url}).Output()
	if err != nil {
		return ""
	}
	var page struct {
		Thumbnails []struct {
			ID         string  `json:"id"`
			URL        string  `json:"url"`
			Width      int     `json:"width"`
			Height     int     `json:"height"`
			Preference float64 `json:"preference"`
		} `json:"thumbnails"`
	}
	if json.Unmarshal(out, &page) != nil {
		return ""
	}
	// Banners rank below zero; a sized square is an avatar, and otherwise the engine lists the best last.
	best, square := "", 0
	for _, t := range page.Thumbnails {
		switch {
		case t.URL == "" || t.Preference < 0:
		case t.Width > 0 && t.Width == t.Height && t.Width > square:
			best, square = t.URL, t.Width
		case square == 0:
			best = t.URL
		}
	}
	return best
}

// Video opens one video for its exact date and details.
func (r *Command) Video(ctx context.Context, url string) (Video, error) {
	if err := r.allowed(ctx, url); err != nil {
		return Video{}, err
	}
	var stderr bytes.Buffer
	c := r.cmd(ctx, r.args.Video, map[string]string{"{url}": url})
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return Video{}, errText(stderr.Bytes(), err)
	}
	var f flat
	if err := json.Unmarshal(out, &f); err != nil {
		return Video{}, err
	}
	return f.video(), nil
}

func selector(quality string) string {
	if quality == "audio" {
		return "bestaudio[ext=m4a]/bestaudio[acodec^=mp4a]/bestaudio/best"
	}
	h := quality
	return fmt.Sprintf("bestvideo[height<=%[1]s][vcodec^=avc1]+bestaudio[ext=m4a]/best[height<=%[1]s][vcodec!=none][acodec!=none]/bestvideo[height<=%[1]s]+bestaudio/best", h)
}

// Resolve asks for the media links of an episode at a quality ("audio", "480", "720", "1080").
func (r *Command) Resolve(ctx context.Context, url, quality string) (Resolved, error) {
	if err := r.allowed(ctx, url); err != nil {
		return Resolved{}, err
	}
	var stderr bytes.Buffer
	c := r.cmd(ctx, r.args.Resolve, map[string]string{"{url}": url, "{quality}": quality, "{format}": selector(quality)})
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return Resolved{}, errText(stderr.Bytes(), err)
	}
	type fmtJSON struct {
		URL         string            `json:"url"`
		Ext         string            `json:"ext"`
		VCodec      string            `json:"vcodec"`
		ACodec      string            `json:"acodec"`
		Headers     map[string]string `json:"http_headers"`
		Filesize    float64           `json:"filesize"`
		FilesizeApx float64           `json:"filesize_approx"`
		Protocol    string            `json:"protocol"`
		Options     struct {
			Chunk int64 `json:"http_chunk_size"`
		} `json:"downloader_options"`
	}
	var v struct {
		fmtJSON
		Duration  float64   `json:"duration"`
		Requested []fmtJSON `json:"requested_formats"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return Resolved{}, err
	}
	conv := func(f fmtJSON) *Format {
		out := &Format{URL: f.URL, Ext: f.Ext, VCodec: f.VCodec, ACodec: f.ACodec, Headers: f.Headers, Size: int64(max(f.Filesize, f.FilesizeApx))}
		if f.Protocol == "https" || f.Protocol == "http" {
			out.Chunk = f.Options.Chunk
		}
		return out
	}
	var f flat
	if err := json.Unmarshal(out, &f); err != nil {
		return Resolved{}, err
	}
	res := Resolved{Duration: int(v.Duration), Expires: time.Now().Add(time.Hour), Details: f.video()}
	switch {
	case len(v.Requested) == 2:
		for _, f := range v.Requested {
			if f.VCodec != "none" && f.VCodec != "" && res.Video == nil {
				res.Video = conv(f)
			} else {
				res.Audio = conv(f)
			}
		}
		if res.Video == nil || res.Audio == nil {
			return res, errors.New("the downloader engine returned no separate video and audio")
		}
	case v.URL != "":
		res.Single = conv(v.fmtJSON)
	default:
		return res, errors.New("the downloader engine found no playable format")
	}
	return res, nil
}

func (r *Command) Version(ctx context.Context) string {
	if len(r.args.Version) == 0 {
		return ""
	}
	out, err := r.cmd(ctx, r.args.Version, nil).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Update runs the engine's own updater; with no update arguments there is nothing to run.
func (r *Command) Update(ctx context.Context) error {
	if len(r.args.Update) == 0 {
		return nil
	}
	if _, err := exec.LookPath(r.bin); err != nil {
		return fmt.Errorf("the downloader engine %q is not installed and PODCASTS_ENGINE_URL is not set", r.bin)
	}
	if out, err := r.cmd(ctx, r.args.Update, nil).CombinedOutput(); err != nil {
		return errText(out, err)
	}
	return nil
}

func first(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
