package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"podcasts/internal/engine"
	"podcasts/internal/status"
	"podcasts/internal/store"
)

// cache holds the files devices play and download, so seeking and resuming read the disk and a second device gets what the first already fetched.
type cache struct {
	dir   string
	log   *slog.Logger
	mu    sync.Mutex
	items map[string]*item
}

// item is one cached file, complete or still being fetched.
type item struct {
	name    string
	episode int64
	etag    string
	stop    context.CancelFunc

	// Guarded by cache.mu.
	used    time.Time
	readers int
	// keep is set once a device downloads it, so its fetch is never stopped to make room.
	keep bool
	// orphan means its seekable copy took its place; the file goes when the last reader leaves.
	orphan bool

	mu      sync.Mutex
	path    string
	mime    string
	ext     string
	written int64
	total   int64
	done    bool
	err     error
	changed chan struct{}
}

var cacheName = regexp.MustCompile(`^((\d+)-\w+)-(\d+)\.(\w+)$`)

// playedGrace is how long a played episode's file stays after its last reader, so replaying it is not cut off.
const playedGrace = time.Minute

// openCache takes over the complete files a previous run left in dir and removes the unfinished ones.
func openCache(dir string, log *slog.Logger) (*cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	c := &cache{dir: dir, log: log, items: map[string]*item{}}
	for _, f := range files {
		path := filepath.Join(dir, f.Name())
		m := cacheName.FindStringSubmatch(f.Name())
		info, err := f.Info()
		if m == nil || m[4] == "part" || err != nil {
			_ = os.Remove(path)
			continue
		}
		episode, _ := strconv.ParseInt(m[2], 10, 64)
		c.items[m[1]] = &item{name: m[1], episode: episode, etag: `"` + m[3] + `"`, stop: func() {}, used: info.ModTime(), path: path, ext: m[4], mime: mimeOfExt(m[4]),
			written: info.Size(), total: info.Size(), done: true, changed: make(chan struct{})}
	}
	return c, nil
}

func (c *cache) blank(name string, episode int64) *item {
	stamp := strconv.FormatInt(time.Now().UnixNano(), 10)
	return &item{name: name, episode: episode, etag: `"` + stamp + `"`, stop: func() {}, path: filepath.Join(c.dir, name+"-"+stamp+".part"), changed: make(chan struct{})}
}

func itemName(episode int64, quality string) string { return fmt.Sprintf("%d-%s", episode, quality) }

// claim returns the episode's item with a reader counted on it; fetch is set when the item is new, and the caller must fill it under that context.
func (c *cache) claim(episode int64, quality string, keep bool) (it *item, fetch context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	name := itemName(episode, quality)
	it, ok := c.items[name]
	if !ok {
		it = c.blank(name, episode)
		fetch, it.stop = context.WithCancel(context.Background())
		c.items[name] = it
	}
	it.used = time.Now()
	it.readers++
	it.keep = it.keep || keep
	return it, fetch
}

func (c *cache) release(it *item) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it.readers--
	it.used = time.Now()
	if it.orphan && it.readers == 0 {
		_ = os.Remove(it.view().path)
	}
}

// complete reports the size of the episode's file when all of it is held.
func (c *cache) complete(episode int64, quality string) (size int64, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.items[itemName(episode, quality)]
	if !ok {
		return 0, false
	}
	v := it.view()
	return v.total, v.done && v.err == nil
}

// preempt stops a fetch nobody is reading or downloading and returns its item, or nil when every fetch is wanted.
func (c *cache) preempt() *item {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, it := range c.items {
		if it.readers == 0 && !it.keep && !it.view().done {
			it.stop()
			return it
		}
	}
	return nil
}

// seekableCopy is an empty item for the rewritten copy of a joined file; finish puts it in the original's place.
func (c *cache) seekableCopy(it *item) *item {
	cp := c.blank(it.name, it.episode)
	cp.mime, cp.ext = it.view().mime, it.view().ext
	return cp
}

// finish closes an item: a failed one is dropped, so the next request starts again; seekable, when given, takes its place.
func (c *cache) finish(it *item, seekable *item, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it.mu.Lock()
	defer it.mu.Unlock()
	it.stop()
	switch {
	case err != nil:
	case seekable != nil:
		final := seekable.path[:len(seekable.path)-len("part")] + seekable.ext
		var info os.FileInfo
		if info, err = os.Stat(seekable.path); err == nil {
			err = os.Rename(seekable.path, final)
		}
		if err == nil {
			seekable.path, seekable.written, seekable.total, seekable.done = final, info.Size(), info.Size(), true
			seekable.used, seekable.keep = time.Now(), it.keep
			c.items[it.name], it.orphan = seekable, true
			if it.readers == 0 {
				_ = os.Remove(it.path)
			}
		}
	default:
		final := it.path[:len(it.path)-len("part")] + it.ext
		if err = os.Rename(it.path, final); err == nil {
			it.path, it.total = final, it.written
		}
	}
	if err != nil {
		_ = os.Remove(it.path)
		if seekable != nil {
			_ = os.Remove(seekable.path)
		}
		if c.items[it.name] == it {
			delete(c.items, it.name)
		}
	}
	it.done, it.err = true, err
	close(it.changed)
	it.changed = make(chan struct{})
}

// sweep drops idle files that are older than maxAge or whose episode is played, then the least recently used ones while the cache is over maxBytes.
func (c *cache) sweep(maxBytes int64, maxAge time.Duration, played map[int64]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var idle []*item
	var held int64
	for _, it := range c.items {
		v := it.view()
		held += v.written
		if v.done && it.readers == 0 {
			idle = append(idle, it)
		}
	}
	sort.Slice(idle, func(i, j int) bool { return idle[i].used.Before(idle[j].used) })
	for _, it := range idle {
		v := it.view()
		old := time.Since(it.used) > maxAge
		heard := played[it.episode] && time.Since(it.used) > playedGrace
		if !old && !heard && held <= maxBytes {
			continue
		}
		_ = os.Remove(v.path)
		delete(c.items, it.name)
		held -= v.written
		c.log.Info("cache: dropped a file", "file", filepath.Base(v.path), "bytes", v.written, "too_old", old, "played", heard)
	}
}

func (c *cache) episodes() []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	ids := make([]int64, 0, len(c.items))
	for _, it := range c.items {
		ids = append(ids, it.episode)
	}
	return ids
}

func (c *cache) usage() (files int, held int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, it := range c.items {
		held += it.view().written
	}
	return len(c.items), held
}

type itemView struct {
	path, mime, ext string
	written, total  int64
	done            bool
	err             error
	changed         chan struct{}
}

func (it *item) view() itemView {
	it.mu.Lock()
	defer it.mu.Unlock()
	return itemView{it.path, it.mime, it.ext, it.written, it.total, it.done, it.err, it.changed}
}

func (it *item) update(set func()) {
	it.mu.Lock()
	set()
	close(it.changed)
	it.changed = make(chan struct{})
	it.mu.Unlock()
}

// wait blocks until the item satisfies until, fails, or ctx ends.
func (it *item) wait(ctx context.Context, until func(itemView) bool) (itemView, error) {
	for {
		v := it.view()
		if v.err != nil || until(v) {
			return v, v.err
		}
		select {
		case <-ctx.Done():
			return v, ctx.Err()
		case <-v.changed:
		}
	}
}

// follow copies bytes from..to (-1: the last) to w, waiting for those still being fetched.
func (it *item) follow(ctx context.Context, w io.Writer, from, to int64) error {
	f, err := os.Open(it.view().path)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 256<<10)
	for pos := from; ; {
		v, err := it.wait(ctx, func(v itemView) bool { return v.done || v.written > pos })
		if err != nil {
			return err
		}
		limit := v.written
		if to >= 0 && to < limit {
			limit = to + 1
		}
		if pos >= limit {
			return nil
		}
		n, err := f.ReadAt(buf[:min(int64(len(buf)), limit-pos)], pos)
		if n == 0 && err != nil {
			return err
		}
		if _, err := w.Write(buf[:n]); err != nil {
			return err
		}
		pos += int64(n)
	}
}

// grow appends what the fetch produces to the item's file and tells its readers.
type grow struct {
	it *item
	f  *os.File
}

func (g grow) Write(p []byte) (int, error) {
	n, err := g.f.Write(p)
	g.it.update(func() { g.it.written += int64(n) })
	return n, err
}

func mimeOfExt(ext string) string {
	switch ext {
	case "webm":
		return "video/webm"
	case "m4a":
		return "audio/mp4"
	case "mp3":
		return "audio/mpeg"
	}
	return "video/mp4"
}

// CacheUsage is what the media cache holds, for the Status page.
type CacheUsage struct {
	Files int
	Bytes int64
}

func (s *Server) CacheUsage() CacheUsage {
	files, held := s.cache.usage()
	return CacheUsage{Files: files, Bytes: held}
}

func (s *Server) sweep(ctx context.Context) {
	set, err := s.st.ServerSettings(ctx)
	if err != nil {
		set = store.DefaultServerSettings()
	}
	played := map[int64]bool{}
	for _, id := range s.cache.episodes() {
		if p, err := s.st.Play(ctx, id); err == nil && p.Played {
			played[id] = true
		}
	}
	s.cache.sweep(int64(set.CacheGB)<<30, time.Duration(set.CacheHours)*time.Hour, played)
}

// serve answers a stream or a download out of the cache, starting the fetch into it when nobody has yet.
func (s *Server) serve(w http.ResponseWriter, r *http.Request, e store.Episode, who string, download bool, offset int) {
	q, err := s.quality(r.Context(), e)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	busy := func(what string) {
		s.log.Warn(what+" refused: too many streams at once", "episode", e.ID, "client", who)
		w.Header().Set("Retry-After", "10")
		http.Error(w, "too many streams at once; try again shortly", http.StatusServiceUnavailable)
	}
	it, fetch := s.cache.claim(e.ID, q, download)
	defer s.cache.release(it)
	if fetch != nil {
		if !s.acquire(r.Context()) && !s.makeRoom(r.Context()) {
			s.cache.finish(it, nil, errors.New("too many streams at once"))
			busy("fetch")
			return
		}
		go s.fill(fetch, it, e)
	}
	s.sweep(r.Context())
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	v, err := it.wait(ctx, func(v itemView) bool { return v.done || v.written > 0 })
	if err != nil {
		if ctx.Err() == nil {
			http.Error(w, "downloader engine: "+err.Error(), http.StatusBadGateway)
		}
		return
	}
	// remote serves from the media host what the cache does not hold yet, as one of the streams at once.
	remote := func(mode string, send func(source, *status.Stream) error) {
		if !s.acquire(ctx) {
			busy("stream")
			return
		}
		defer s.release()
		res, err := s.formats(ctx, e)
		if err != nil {
			s.end(s.track.AddStream(e.ID, e.Title, "resolve", who, offset, cancel), err)
			http.Error(w, "downloader engine: "+err.Error(), http.StatusBadGateway)
			return
		}
		st := s.track.AddStream(e.ID, e.Title, mode, who, offset, cancel)
		s.log.Info("stream started", "id", st.ID, "episode", e.ID, "mode", mode, "offset", offset, "range", r.Header.Get("Range"), "client", who)
		err = send(res, st)
		if ctx.Err() != nil {
			err = nil
		}
		if err != nil {
			s.forget(e)
		}
		s.end(st, err)
	}
	local := func(send func(*status.Stream) error) {
		st := s.track.AddStream(e.ID, e.Title, "from cache", who, offset, cancel)
		s.log.Info("stream started", "id", st.ID, "episode", e.ID, "mode", "from cache", "offset", offset, "range", r.Header.Get("Range"), "cached", v.written, "complete", v.done, "download", download, "client", who)
		err := send(st)
		if ctx.Err() != nil {
			err = nil
		}
		s.end(st, err)
		if err != nil {
			// Cut the connection, so the device sees a failure and not a short file.
			panic(http.ErrAbortHandler)
		}
	}
	if offset > 0 && !download {
		if !v.done && !s.reached(ctx, e, v.written, offset) {
			remote("joined", func(res source, st *status.Stream) error { return s.join(ctx, w, res, offset, st) })
			return
		}
		local(func(st *status.Stream) error { return s.cut(ctx, w, it, e, offset, st) })
		return
	}
	start, end, ranged := byteRange(r.Header.Get("Range"))
	if match := r.Header.Get("If-Range"); match != "" && match != it.etag {
		ranged = false
	}
	if !download && ranged && v.total > 0 && !v.done && start > v.written {
		remote("pass-through", func(res source, st *status.Stream) error {
			if res.Single == nil {
				return errors.New("the formats changed while this was being fetched")
			}
			return s.passThrough(ctx, w, r, res.Single, st)
		})
		return
	}
	if download && ranged && start > 0 && v.total == 0 {
		// A range can't be answered until the size is known; a joined file has none before it is finished.
		soon, stop := context.WithTimeout(ctx, 20*time.Second)
		v, _ = it.wait(soon, func(v itemView) bool { return v.done })
		stop()
		if v.err != nil {
			http.Error(w, "downloader engine: "+v.err.Error(), http.StatusBadGateway)
			return
		}
	}
	if !ranged || v.total == 0 || start >= v.total {
		start, end, ranged = 0, -1, false
	}
	if end < 0 || end >= v.total {
		end = v.total - 1
	}
	h := w.Header()
	h.Set("ETag", it.etag)
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Type", v.mime)
	if download {
		h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="episode-%d.%s"`, e.ID, v.ext))
	}
	if v.total > 0 {
		h.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	} else if !download {
		joinedHead(h, 0, e.Duration)
	}
	if ranged {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, v.total))
		w.WriteHeader(http.StatusPartialContent)
	}
	local(func(st *status.Stream) error { return it.follow(ctx, counter{w, st}, start, end) })
}

// makeRoom stops a fetch nobody is waiting for and takes its place among the streams at once.
func (s *Server) makeRoom(ctx context.Context) bool {
	idle := s.cache.preempt()
	if idle == nil {
		return false
	}
	_, _ = idle.wait(ctx, func(v itemView) bool { return v.done })
	return s.acquire(ctx)
}

// reached guesses, from its expected size and length, whether a joined file being fetched already holds offset seconds.
func (s *Server) reached(ctx context.Context, e store.Episode, written int64, offset int) bool {
	res, err := s.formats(ctx, e)
	if err != nil || res.Single != nil {
		return true
	}
	size := res.size()
	return size > 0 && res.Duration > 0 && int64(offset)*size/int64(res.Duration) <= written
}

// cut sends the cached file from offset seconds on as fragmented MP4, following a file that is still being fetched.
func (s *Server) cut(ctx context.Context, w http.ResponseWriter, it *item, e store.Episode, offset int, st *status.Stream) error {
	joinedHead(w.Header(), offset, e.Duration)
	v, at := it.view(), strconv.Itoa(offset)
	args := []string{"-nostdin", "-ss", at, "-i", v.path}
	var feed func(context.Context, io.Writer) error
	if v.total == 0 {
		// A file without an index in front can only be read from its start, so ffmpeg skips to the offset itself.
		args = []string{"-i", "pipe:0", "-ss", at}
		feed = func(ctx context.Context, in io.Writer) error { return it.follow(ctx, in, 0, -1) }
	}
	frag := fragmented
	if strings.HasPrefix(v.mime, "audio/") {
		frag = fragmentedAudio
	}
	return s.run(ctx, counter{w, st}, append(append(args, "-map", "0", "-c", "copy"), frag...), feed)
}

// fill fetches an episode into its cache item; it runs on after the device that asked has gone, so the next request finds it.
func (s *Server) fill(fetch context.Context, it *item, e store.Episode) {
	ctx, cancel := context.WithCancel(fetch)
	defer cancel()
	st := s.track.AddStream(e.ID, e.Title, "caching", "media cache", 0, cancel)
	seekable, err := s.fetch(ctx, it, e, st)
	if errors.Is(err, context.Canceled) {
		err = errors.New("stopped")
	}
	if err != nil {
		s.forget(e)
	}
	// A fetch stopped to make room for another is not a failure.
	madeRoom := fetch.Err() != nil
	s.release()
	s.cache.finish(it, seekable, err)
	if madeRoom {
		s.end(st, nil)
	} else {
		s.end(st, err)
	}
	s.sweep(context.Background())
}

// fetch fills the item's file; a joined file also comes back as a seekable copy to put in its place.
func (s *Server) fetch(ctx context.Context, it *item, e store.Episode, st *status.Stream) (seekable *item, err error) {
	res, err := s.formats(ctx, e)
	if err != nil {
		return nil, err
	}
	f, err := os.Create(it.view().path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := counter{grow{it, f}, st}
	one := res.Single
	if one == nil {
		it.update(func() { it.mime, it.ext = "video/mp4", "mp4" })
		if res.encode == "audio" {
			it.update(func() { it.mime, it.ext = "audio/mp4", "m4a" })
		}
		if err := s.mux(ctx, out, res, 0); err != nil {
			return nil, err
		}
		// Fragments let devices follow the fetch; the index in front lets them seek by byte range afterwards.
		cp := s.cache.seekableCopy(it)
		err := s.run(ctx, io.Discard, []string{"-nostdin", "-i", it.view().path, "-map", "0", "-c", "copy", "-movflags", "+faststart", "-f", "mp4", "-y", cp.path}, nil)
		return cp, err
	}
	ext := one.Ext
	if ext == "" {
		ext = "mp4"
	}
	it.update(func() { it.mime, it.ext = mimeOf(one), ext })
	if one.Chunk > 0 {
		return nil, s.chunked(ctx, out, one, 0, -1, func(_, total int64) { it.update(func() { it.total = total }) })
	}
	return nil, s.whole(ctx, out, one, it)
}

// whole fetches a file from a host that serves it at full speed in one request.
func (s *Server) whole(ctx context.Context, out io.Writer, f *engine.Format, it *item) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return err
	}
	for k, v := range f.Headers {
		req.Header.Set(k, v)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("media host: %s", resp.Status)
	}
	if resp.ContentLength > 0 {
		it.update(func() { it.total = resp.ContentLength })
	}
	_, err = io.Copy(out, resp.Body)
	return err
}
