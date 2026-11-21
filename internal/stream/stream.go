// Package stream serves video episodes on demand through the downloader engine, passing one file through or joining video and audio with ffmpeg.
package stream

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"podcasts/internal/engine"
	"podcasts/internal/feed"
	"podcasts/internal/status"
	"podcasts/internal/store"
)

const linkTTL = 6 * time.Hour

type Server struct {
	st      *store.Store
	engine  engine.Engine
	track   *status.Tracker
	log     *slog.Logger
	http    *http.Client
	key     []byte
	ffmpeg  string
	ffprobe string
	guard   feed.Guard
	ensure  func(context.Context)
	mu      sync.Mutex
	active  int
	cache   *cache
	links   map[string]cached
	resolve sync.Map
	// inner is the loopback address ffmpeg reads chunked inputs from, by the tokens in inputs.
	inner  string
	inputs sync.Map
}

type cached struct {
	res     source
	expires time.Time
}

type Options struct {
	MasterKey []byte
	FFmpeg    string
	FFprobe   string
	// Client fetches media links, and its guard says which ones may be given to ffmpeg.
	Client       *feed.Client
	EnsureEngine func(context.Context)
	// CacheDir is the folder the media cache keeps its files in.
	CacheDir string
}

// New also opens the media cache, which is swept until ctx ends.
func New(ctx context.Context, st *store.Store, eng engine.Engine, track *status.Tracker, log *slog.Logger, o Options) (*Server, error) {
	mac := hmac.New(sha256.New, o.MasterKey)
	mac.Write([]byte("stream links"))
	c, err := openCache(o.CacheDir, log)
	if err != nil {
		return nil, fmt.Errorf("media cache: %w", err)
	}
	s := &Server{st: st, engine: eng, track: track, log: log, http: o.Client.HTTP, key: mac.Sum(nil), ffmpeg: o.FFmpeg, ffprobe: o.FFprobe, guard: o.Client.Guard, ensure: o.EnsureEngine, cache: c, links: map[string]cached{}}
	if err := s.listenInner(); err != nil {
		return nil, err
	}
	go func() {
		tick := time.NewTicker(10 * time.Minute)
		defer tick.Stop()
		for {
			s.sweep(ctx)
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return s, nil
}

func (s *Server) sign(id int64, exp int64) string {
	m := hmac.New(sha256.New, s.key)
	fmt.Fprintf(m, "%d:%d", id, exp)
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// Link returns a signed stream address that works without a cookie for 6 hours.
func (s *Server) Link(id int64, download bool) string {
	exp := time.Now().Add(linkTTL).Unix()
	q := url.Values{"exp": {strconv.FormatInt(exp, 10)}, "sig": {s.sign(id, exp)}}
	if download {
		q.Set("download", "1")
	}
	return fmt.Sprintf("/stream/%d?%s", id, q.Encode())
}

func (s *Server) valid(id int64, q url.Values) bool {
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(q.Get("sig")), []byte(s.sign(id, exp)))
}

// Prepared is what a player needs to know before it opens a stream.
type Prepared struct {
	// Joined means seeking restarts the stream; a file the cache holds whole is never joined.
	Joined bool
	// Size in bytes and Duration in seconds are 0 when unknown.
	Size     int64
	Duration int
}

// Prepare resolves an episode's formats ahead of playback, so errors show before the player starts.
func (s *Server) Prepare(ctx context.Context, e store.Episode) (Prepared, error) {
	if !e.Streamed() {
		return Prepared{Size: e.Size, Duration: e.Duration}, nil
	}
	if q, err := s.quality(ctx, e); err == nil && e.Duration > 0 {
		if size, ok := s.cache.complete(e.ID, q); ok {
			return Prepared{Size: size, Duration: e.Duration}, nil
		}
	}
	res, err := s.formats(ctx, e)
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{Joined: res.Single == nil, Duration: max(res.Duration, e.Duration), Size: res.size()}, nil
}

// quality is what the engine is asked for, "audio", "480", "720" or "1080": the channel's own, or else the server's default.
func (s *Server) quality(ctx context.Context, e store.Episode) (string, error) {
	if !e.Direct() && e.PageURL == "" {
		return "", errors.New("this episode has no web page to stream from")
	}
	ch, err := s.st.Channel(ctx, e.ChannelID)
	if err != nil {
		return "", err
	}
	set, err := s.st.ServerSettings(ctx)
	if err != nil {
		return "", err
	}
	q := ch.Effective(set).Quality
	if e.Media == "audio" && q != "audio" && ch.Kind == "feed" {
		q = "audio"
	}
	return q, nil
}

func (s *Server) formats(ctx context.Context, e store.Episode) (source, error) {
	q, err := s.quality(ctx, e)
	if err != nil {
		return source{}, err
	}
	key := fmt.Sprintf("%d:%s", e.ID, q)
	s.mu.Lock()
	c, ok := s.links[key]
	s.mu.Unlock()
	if ok && time.Now().Before(c.expires) {
		return c.res, nil
	}
	lock, _ := s.resolve.LoadOrStore(key, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()
	s.mu.Lock()
	c, ok = s.links[key]
	s.mu.Unlock()
	if ok && time.Now().Before(c.expires) {
		return c.res, nil
	}
	var res source
	if e.Direct() {
		res, err = s.enclosure(ctx, e, q)
	} else {
		res, err = s.resolveLinks(ctx, e, q)
	}
	if err != nil {
		return res, err
	}
	s.mu.Lock()
	for k, v := range s.links {
		if time.Now().After(v.expires) {
			delete(s.links, k)
		}
	}
	s.links[key] = cached{res: res, expires: res.Expires}
	s.mu.Unlock()
	return res, nil
}

// resolveLinks asks the downloader engine for the episode's links; they expire, and so does the answer.
func (s *Server) resolveLinks(ctx context.Context, e store.Episode, q string) (source, error) {
	if s.ensure != nil {
		s.ensure(ctx)
	}
	res, err := s.engine.Resolve(ctx, e.PageURL, q)
	if err != nil {
		return source{}, err
	}
	// Resolving opens the video, so its exact date, show notes and chapters come for free.
	if err := s.st.SetExactDetails(ctx, e.ID, store.DetailsOf(res.Details)); err != nil {
		s.log.Warn("video details", "episode", e.ID, "err", err)
	}
	exp := res.Expires
	for _, f := range []*engine.Format{res.Single, res.Video, res.Audio} {
		if f == nil {
			continue
		}
		if u, err := url.Parse(f.URL); err == nil {
			if v, err := strconv.ParseInt(u.Query().Get("expire"), 10, 64); err == nil {
				if t := time.Unix(v, 0).Add(-5 * time.Minute); t.Before(exp) {
					exp = t
				}
			}
		}
	}
	res.Expires = exp
	return source{Resolved: res}, nil
}

func (s *Server) forget(e store.Episode) {
	s.mu.Lock()
	for k := range s.links {
		if strings.HasPrefix(k, fmt.Sprintf("%d:", e.ID)) {
			delete(s.links, k)
		}
	}
	s.mu.Unlock()
}

func (s *Server) acquire(ctx context.Context) bool {
	set, err := s.st.ServerSettings(ctx)
	if err != nil {
		set = store.DefaultServerSettings()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active >= set.Streams {
		return false
	}
	s.active++
	return true
}

func (s *Server) release() {
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
}

func client(r *http.Request) string {
	ua := r.UserAgent()
	kind := "browser"
	if strings.Contains(ua, "Dart") || strings.Contains(ua, "podcasts-app") || strings.Contains(ua, "ExoPlayer") || strings.Contains(ua, "AppleCoreMedia") || strings.Contains(ua, "libmpv") {
		kind = "app"
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return kind + " · " + host
}

// ServeHTTP handles GET /stream/{id}?exp=&sig=[&t=seconds][&download=1].
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || !s.valid(id, r.URL.Query()) {
		s.log.Warn("stream link refused: not valid or expired", "path", r.URL.Path, "client", client(r))
		http.Error(w, "this stream link is not valid or has expired", http.StatusForbidden)
		return
	}
	e, err := s.st.Episode(r.Context(), id)
	if err != nil {
		http.Error(w, "episode not found", http.StatusNotFound)
		return
	}
	if !e.Streamed() {
		http.Redirect(w, r, e.MediaURL, http.StatusFound)
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("t"))
	s.serve(w, r, e, client(r), r.URL.Query().Get("download") == "1", offset)
}

func (s *Server) end(st *status.Stream, err error) {
	v := s.track.EndStream(st, err)
	args := []any{"id", v.ID, "episode", v.EpisodeID, "mode", v.Mode, "offset", v.Offset, "bytes", v.Bytes, "elapsed", v.Elapsed(), "client", v.Client}
	if err != nil {
		s.log.Error("stream failed", append(args, "err", err)...)
		return
	}
	s.log.Info("stream ended", args...)
}

type counter struct {
	w  io.Writer
	st *status.Stream
}

func (c counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.st.Bytes.Add(int64(n))
	return n, err
}

func (s *Server) passThrough(ctx context.Context, w http.ResponseWriter, r *http.Request, f *engine.Format, st *status.Stream) error {
	if start, end, ranged := byteRange(r.Header.Get("Range")); f.Chunk > 0 && (ranged || r.Header.Get("Range") == "") {
		if !ranged {
			end = -1
		}
		return s.serveChunked(ctx, w, counter{w, st}, f, start, end, ranged)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return err
	}
	for k, v := range f.Headers {
		req.Header.Set(k, v)
	}
	if rg := r.Header.Get("Range"); rg != "" {
		req.Header.Set("Range", rg)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		http.Error(w, "the media host answered "+resp.Status, http.StatusBadGateway)
		return fmt.Errorf("media host: %s", resp.Status)
	}
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if w.Header().Get("Accept-Ranges") == "" {
		w.Header().Set("Accept-Ranges", "bytes")
	}
	// A media host must not get a page of its own served from this origin.
	if ct := strings.ToLower(w.Header().Get("Content-Type")); !strings.HasPrefix(ct, "audio/") && !strings.HasPrefix(ct, "video/") && !strings.Contains(ct, "mpegurl") {
		w.Header().Set("Content-Type", mimeOf(f))
	}
	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(counter{w, st}, resp.Body)
	return err
}

func mimeOf(f *engine.Format) string {
	audioOnly := f.VCodec == "none" || f.VCodec == ""
	switch f.Ext {
	case "webm":
		if audioOnly {
			return "audio/webm"
		}
		return "video/webm"
	case "m4a":
		return "audio/mp4"
	case "mp3":
		return "audio/mpeg"
	}
	if audioOnly && f.ACodec != "" && f.ACodec != "none" {
		return "audio/mp4"
	}
	return "video/mp4"
}

func headerArg(h map[string]string) []string {
	if len(h) == 0 {
		return nil
	}
	var b strings.Builder
	for k, v := range h {
		b.WriteString(k + ": " + v + "\r\n")
	}
	return []string{"-headers", b.String()}
}

// join sends separate video and audio links as one fragmented MP4; seeking restarts it at offset seconds.
func (s *Server) join(ctx context.Context, w http.ResponseWriter, res source, offset int, st *status.Stream) error {
	joinedHead(w.Header(), offset, res.Duration)
	err := s.mux(ctx, counter{w, st}, res, offset)
	if err != nil && st.Bytes.Load() == 0 && ctx.Err() == nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
	}
	return err
}

// joinedHead describes a fragmented MP4 that starts offset seconds in and can't be seeked by byte range.
func joinedHead(h http.Header, offset, duration int) {
	h.Set("Content-Type", "video/mp4")
	h.Set("Accept-Ranges", "none")
	h.Set("X-Stream-Offset", strconv.Itoa(offset))
	if duration > 0 {
		h.Set("X-Content-Duration", strconv.Itoa(duration))
	}
}

var fragmented = []string{"-movflags", "frag_keyframe+empty_moov+default_base_moof", "-f", "mp4", "pipe:1"}

// fragmentedAudio cuts by time, as a fragment per audio packet comes out unreadable.
var fragmentedAudio = []string{"-movflags", "empty_moov+default_base_moof", "-frag_duration", "1000000", "-f", "mp4", "pipe:1"}

// mux writes the video and audio links, or the one link it re-encodes, to w as fragmented MP4, made by ffmpeg on the fly.
func (s *Server) mux(ctx context.Context, w io.Writer, res source, offset int) error {
	args := []string{"-nostdin"}
	inputs, out := []*engine.Format{res.Video, res.Audio}, []string{"-map", "0:v:0", "-map", "1:a:0", "-c", "copy"}
	if res.encode != "" {
		inputs, out = []*engine.Format{res.Video}, encodeArgs(res.encode, res.Video.ACodec)
	}
	for _, f := range inputs {
		// ffmpeg dials for itself, past the client and its guard.
		if err := s.guard.Web(ctx, f.URL); err != nil {
			return fmt.Errorf("media link: %w", err)
		}
		addr, headers, done := s.input(f)
		defer done()
		args = append(args, "-protocol_whitelist", "http,https,tcp,tls,crypto")
		if offset > 0 {
			args = append(args, "-ss", strconv.Itoa(offset))
		}
		args = append(args, headers...)
		args = append(args, "-reconnect", "1", "-reconnect_streamed", "1", "-i", addr)
	}
	frag := fragmented
	if res.encode == "audio" {
		frag = fragmentedAudio
	}
	return s.run(ctx, w, append(append(args, out...), frag...), nil)
}

// run copies what ffmpeg writes to its standard output into w; feed, when given, supplies its standard input.
func (s *Server) run(ctx context.Context, w io.Writer, args []string, feed func(context.Context, io.Writer) error) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	cmd := exec.CommandContext(ctx, s.ffmpeg, append([]string{"-hide_banner", "-loglevel", "error"}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if feed != nil {
		in, err := cmd.StdinPipe()
		if err != nil {
			return err
		}
		go func() {
			_ = feed(ctx, in)
			in.Close()
		}()
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	_, copyErr := io.Copy(w, out)
	// ffmpeg would wait for ever on a pipe nobody reads.
	out.Close()
	werr := cmd.Wait()
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case copyErr != nil:
		return copyErr
	case werr != nil:
		return fmt.Errorf("ffmpeg: %v %s", werr, strings.TrimSpace(stderr.String()))
	}
	return nil
}
