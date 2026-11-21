// Package web serves the htmx UI: playlists, podcasts, episodes, the player, status, devices and settings.
package web

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"podcasts/internal/feed"
	"podcasts/internal/intake"
	"podcasts/internal/jobs"
	"podcasts/internal/library"
	"podcasts/internal/signin"
	"podcasts/internal/status"
	"podcasts/internal/store"
	"podcasts/internal/stream"
)

//go:embed templates static
var assets embed.FS

const (
	sessionCookie = "podcasts_session"
	sessionTTL    = 30 * 24 * time.Hour
)

type Options struct {
	Password     string
	MasterKey    []byte
	SecureCookie bool
	// Demo refuses every change; only signing in and out work.
	Demo bool
	// LockSettings refuses changes to Settings → Server and greys the form out.
	LockSettings bool
}

type Server struct {
	st      *store.Store
	lib     *library.Loader
	jobs    *jobs.Manager
	track   *status.Tracker
	streams *stream.Server
	feed    *feed.Client
	gate    *signin.Gate
	log     *slog.Logger
	o       Options
	key     []byte
	pages   map[string]*template.Template
	chap    *chapterCache
}

func New(st *store.Store, lib *library.Loader, jm *jobs.Manager, track *status.Tracker, ss *stream.Server, fc *feed.Client, gate *signin.Gate, log *slog.Logger, o Options) (*Server, error) {
	mac := hmac.New(sha256.New, o.MasterKey)
	mac.Write([]byte("session:" + o.Password))
	s := &Server{st: st, lib: lib, jobs: jm, track: track, streams: ss, feed: fc, gate: gate, log: log, o: o, key: mac.Sum(nil), pages: map[string]*template.Template{}, chap: newChapterCache()}
	base, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/*.tmpl")
	if err != nil {
		return nil, err
	}
	pages, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, p := range pages {
		t, err := base.Clone()
		if err != nil {
			return nil, err
		}
		if t, err = t.ParseFS(assets, p); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		s.pages[strings.TrimSuffix(path.Base(p), ".html")] = t
	}
	return s, nil
}

var assetVersion = func() string {
	h := sha256.New()
	_ = fs.WalkDir(assets, "static", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := assets.ReadFile(p)
			h.Write(b)
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}()

func clock(secs int) string {
	if secs < 0 {
		secs = 0
	}
	h, m, s := secs/3600, secs/60%60, secs%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func length(secs int) string {
	switch {
	case secs <= 0:
		return ""
	case secs < 3600:
		return fmt.Sprintf("%d min", (secs+30)/60)
	}
	return fmt.Sprintf("%dh %02dm", secs/3600, secs/60%60)
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	future := d < 0
	if future {
		d = -d
	}
	var s string
	switch {
	case d < 10*time.Second:
		return "just now"
	case d < time.Minute:
		s = fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		s = fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		s = fmt.Sprintf("%dh", int(d.Hours()))
	default:
		s = fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	if future {
		return "in " + s
	}
	return s + " ago"
}

var funcs = template.FuncMap{
	"asset":  func(name string) string { return "/static/" + name + "?v=" + assetVersion },
	"ago":    ago,
	"clock":  clock,
	"length": length,
	"date": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.Format("2 Jan 2006")
	},
	"datetime": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.Format("2 Jan 2006 15:04")
	},
	"dur": func(d time.Duration) string {
		switch {
		case d <= 0:
			return "0s"
		case d < time.Minute:
			return fmt.Sprintf("%ds", int(d.Seconds()))
		case d < time.Hour:
			return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
		}
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	},
	"bytes": func(n int64) string {
		const unit = 1024
		if n < unit {
			return fmt.Sprintf("%d B", n)
		}
		div, exp := int64(unit), 0
		for m := n / unit; m >= unit; m /= unit {
			div *= unit
			exp++
		}
		return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
	},
	"pct": func(a, b int) int {
		if b <= 0 {
			return 0
		}
		return min(100, a*100/b)
	},
	"join": strings.Join,
	"add":  func(a, b int) int { return a + b },
	"seq": func(n int) []int {
		out := make([]int, n)
		for i := range out {
			out[i] = i + 1
		}
		return out
	},
	"json": func(v any) template.JS {
		b, _ := json.Marshal(v)
		return template.JS(b)
	},
	"qs": func(kv ...string) template.URL {
		q := url.Values{}
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i+1] != "" {
				q.Set(kv[i], kv[i+1])
			}
		}
		if len(q) == 0 {
			return ""
		}
		return template.URL("?" + q.Encode())
	},
	"itoa": strconv.Itoa,
}

// page is what every full page template receives.
type page struct {
	Title   string
	Nav     string
	Flash   string
	Error   string
	Crumbs  []crumb
	Theme   string
	Busy    int
	Failed  bool
	Boosted bool
	Data    any
}

type crumb struct{ Label, Href string }

func (s *Server) render(w http.ResponseWriter, r *http.Request, code int, name string, p page) {
	ctx := r.Context()
	if p.Flash == "" {
		p.Flash = r.URL.Query().Get("flash")
	}
	if p.Error == "" {
		p.Error = r.URL.Query().Get("error")
	}
	if prefs, err := s.st.Prefs(ctx); err == nil {
		p.Theme = prefs.Theme
	}
	p.Busy = s.track.Busy() + len(s.jobs.Queued())
	p.Failed = s.st.LastFailed(ctx) || s.track.StreamFailed()
	p.Boosted = r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-History-Restore-Request") != "true" && r.Header.Get("HX-Target") == "page"
	s.execute(w, code, name, name+".html", p)
}

func (s *Server) partial(w http.ResponseWriter, name, block string, data any) {
	s.execute(w, http.StatusOK, name, block, data)
}

func (s *Server) execute(w http.ResponseWriter, code int, name, block string, data any) {
	t, ok := s.pages[name]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, block, data); err != nil {
		s.log.Error("render", "page", name, "block", block, "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_, _ = buf.WriteTo(w)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	code := http.StatusInternalServerError
	msg := "Something went wrong: " + err.Error()
	if err == store.ErrNotFound {
		code, msg = http.StatusNotFound, "Not found. It may have been deleted."
	} else {
		s.log.Error("request", "path", r.URL.Path, "err", err)
	}
	s.render(w, r, code, "error", page{Title: "Error", Error: msg})
}

// back redirects with a flash or error message, so every POST ends in a GET.
func back(w http.ResponseWriter, r *http.Request, target, flash string, err error) {
	q := url.Values{}
	if err != nil {
		q.Set("error", err.Error())
	} else if flash != "" {
		q.Set("flash", flash)
	}
	if len(q) > 0 {
		if strings.Contains(target, "?") {
			target += "&" + q.Encode()
		} else {
			target += "?" + q.Encode()
		}
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Location", fmt.Sprintf(`{"path":%q,"target":"#page","select":"#page","swap":"outerHTML"}`, target))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) sign(v string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(v))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) validSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	exp, sig, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.sign(exp))) {
		return false
	}
	n, err := strconv.ParseInt(exp, 10, 64)
	return err == nil && time.Now().Unix() < n
}

// requireLogin also refuses cross-site posts, which the Lax cookie alone would allow from same-site pages.
func (s *Server) requireLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The session comes first, so nobody signed out gets a form read into memory.
		if !s.validSession(r) {
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", "/login")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost {
			if o := r.Header.Get("Origin"); o != "" {
				if u, err := url.Parse(o); err != nil || u.Host != r.Host {
					http.Error(w, "cross-origin request refused", http.StatusForbidden)
					return
				}
			}
			r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
			if err := r.ParseMultipartForm(16 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "login", page{Title: "Sign in", Nav: "login"})
}

// secure marks the cookie HTTPS-only when set to, and by itself whenever the request came over HTTPS.
func (s *Server) secure(r *http.Request) bool {
	return s.o.SecureCookie || r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	// Anyone can send this, so it gets a small body and little time to send it.
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(20 * time.Second))
	refuse := func(code int, msg string) {
		s.render(w, r, code, "login", page{Title: "Sign in", Nav: "login", Error: msg})
	}
	switch s.gate.Check(intake.Client(r.RemoteAddr, r.Header.Values("X-Forwarded-For")), r.PostFormValue("password")) {
	case signin.Throttled:
		refuse(http.StatusTooManyRequests, "Too many wrong passwords. Wait a minute and try again.")
		return
	case signin.Wrong:
		refuse(http.StatusUnauthorized, "Wrong password")
		return
	}
	exp := strconv.FormatInt(time.Now().Add(sessionTTL).Unix(), 10)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: exp + "." + s.sign(exp), Path: "/", HttpOnly: true,
		Secure: s.secure(r), SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds())})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secure(r)})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src * data:; media-src * blob:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// Routes registers the UI; /stream and /api are registered by their own packages.
func (s *Server) Routes(mux *http.ServeMux) {
	static, _ := fs.Sub(assets, "static")
	files := http.FileServer(http.FS(static))
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		files.ServeHTTP(w, r)
	})))
	mux.Handle("GET /login", security(http.HandlerFunc(s.loginPage)))
	mux.Handle("POST /login", security(http.HandlerFunc(s.login)))
	auth := func(pattern string, h http.HandlerFunc) {
		if s.o.Demo && !strings.HasPrefix(pattern, "GET ") && pattern != "POST /logout" {
			h = func(w http.ResponseWriter, r *http.Request) {
				target := "/"
				if u, err := url.Parse(r.Referer()); err == nil && strings.HasPrefix(u.Path, "/") {
					target = u.Path
				}
				back(w, r, target, "", errors.New("This is a read-only demo server."))
			}
		}
		mux.Handle(pattern, security(s.requireLogin(h)))
	}
	auth("POST /logout", s.logout)

	auth("GET /{$}", s.playlistsPage)
	auth("GET /playlists/new", s.playlistNew)
	auth("POST /playlists", s.playlistCreate)
	auth("POST /playlists/move", s.playlistMove)
	auth("GET /playlists/{id}", s.playlistPage)
	auth("GET /playlists/{id}/edit", s.playlistEdit)
	auth("POST /playlists/{id}", s.playlistUpdate)
	auth("POST /playlists/{id}/delete", s.playlistDelete)
	auth("POST /playlists/{id}/mark", s.playlistMark)
	auth("POST /playlists/{id}/fix-dates", s.playlistFixDates)
	auth("POST /playlists/{id}/order", s.playlistOrder)
	auth("POST /playlists/{id}/reshuffle", s.playlistReshuffle)

	auth("GET /podcasts", s.podcastsPage)
	auth("GET /podcasts/add", s.addPage)
	auth("GET /podcasts/add/preview", s.addPreview)
	auth("POST /podcasts/add/preview", s.addPreview)
	auth("POST /podcasts/add", s.addChannel)
	auth("POST /podcasts/check-all", s.checkAll)
	auth("GET /podcasts/opml", s.opmlExport)
	auth("POST /podcasts/opml", s.opmlImport)
	auth("GET /tags", s.tagsPage)
	auth("POST /tags/rename", s.tagRename)
	auth("POST /tags/playlist", s.tagPlaylist)

	auth("GET /channels/{id}", s.channelPage)
	auth("GET /channels/{id}/settings", s.channelSettingsPage)
	auth("POST /channels/{id}/settings", s.channelSettings)
	auth("POST /channels/{id}/check", s.channelCheck)
	auth("POST /channels/{id}/readback", s.channelReadBack)
	auth("POST /channels/{id}/mark-played", s.channelMarkPlayed)
	auth("POST /channels/{id}/unfollow", s.channelUnfollow)

	auth("GET /episodes", s.episodesPage)
	auth("POST /episodes/select", s.episodesSelect)
	auth("GET /episodes/{id}", s.episodePage)
	auth("POST /episodes/{id}/action", s.episodeAction)
	auth("GET /episodes/{id}/play", s.episodePlay)
	auth("GET /episodes/{id}/chapters", s.episodeChapters)

	auth("GET /filter/check", s.filterCheck)
	auth("GET /filter/options", s.filterOptions)

	auth("GET /status", s.statusPage)
	auth("GET /status/live", s.statusLive)
	auth("GET /status/badge", s.statusBadge)
	auth("POST /status/jobs/{id}/cancel", s.jobCancel)
	auth("POST /status/jobs/{id}/retry", s.jobRetry)
	auth("POST /status/streams/{id}/stop", s.streamStop)
	auth("POST /status/engine", s.engineUpdate)

	auth("GET /devices", s.devicesPage)
	auth("POST /devices/{id}/forget", s.deviceForget)

	auth("GET /settings", s.settingsPage)
	auth("POST /settings/prefs", s.savePrefs)
	saveServer := s.saveServer
	if s.o.LockSettings {
		saveServer = func(w http.ResponseWriter, r *http.Request) {
			back(w, r, "/settings", "", errors.New("Server settings are locked on this server."))
		}
	}
	auth("POST /settings/server", saveServer)
}

func (s *Server) view(ctx context.Context) (*library.View, error) { return s.lib.Load(ctx) }
