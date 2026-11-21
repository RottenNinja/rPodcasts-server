// Package api serves the JSON API under /api/v1 for the app; calls carry the device token as a bearer token.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"podcasts/internal/feed"
	"podcasts/internal/filter"
	"podcasts/internal/intake"
	"podcasts/internal/jobs"
	"podcasts/internal/library"
	"podcasts/internal/signin"
	"podcasts/internal/status"
	"podcasts/internal/store"
	"podcasts/internal/stream"
)

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
}

type Options struct {
	// Demo refuses every write except sign-in and forgetting this device; sync pushes are dropped.
	Demo bool
	// LockSettings refuses changes to the server defaults.
	LockSettings bool
}

func New(st *store.Store, lib *library.Loader, jm *jobs.Manager, track *status.Tracker, ss *stream.Server, fc *feed.Client, gate *signin.Gate, log *slog.Logger, o Options) *Server {
	return &Server{st: st, lib: lib, jobs: jm, track: track, streams: ss, feed: fc, gate: gate, log: log, o: o}
}

type ctxKey struct{}

func device(ctx context.Context) store.Device { d, _ := ctx.Value(ctxKey{}).(store.Device); return d }

type apiError struct {
	code int
	msg  string
}

func (e apiError) Error() string { return e.msg }

func bad(err error) error { return apiError{http.StatusBadRequest, err.Error()} }

type handler func(r *http.Request, in url.Values) (any, error)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// values reads a JSON object body into form-like values, so intake validates API bodies and forms alike.
func values(r *http.Request) (url.Values, error) {
	out := url.Values{}
	for k, v := range r.URL.Query() {
		out[k] = v
	}
	if r.Method == http.MethodGet || r.Body == nil {
		return out, nil
	}
	var m map[string]any
	dec := json.NewDecoder(io.LimitReader(r.Body, 8<<20))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil && !errors.Is(err, io.EOF) {
		return nil, bad(errors.New("body: want a JSON object"))
	}
	for k, v := range m {
		switch x := v.(type) {
		case string:
			out.Set(k, x)
		case json.Number:
			out.Set(k, x.String())
		case bool:
			out.Set(k, strconv.FormatBool(x))
		case []any:
			for _, e := range x {
				switch y := e.(type) {
				case string:
					out.Add(k, y)
				case json.Number:
					out.Add(k, y.String())
				default:
					b, _ := json.Marshal(y)
					out.Add(k, string(b))
				}
			}
		case nil:
		default:
			b, _ := json.Marshal(x)
			out.Set(k, string(b))
		}
	}
	return out, nil
}

func (s *Server) wrap(h handler, auth bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth {
			tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			d, err := s.st.DeviceByToken(r.Context(), strings.TrimSpace(tok))
			if !ok || err != nil {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in again"})
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, d))
		} else {
			// Anyone can send this, so it gets a small body and little time to send it.
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(20 * time.Second))
		}
		in, err := values(r)
		var out any
		if err == nil {
			out, err = h(r, in)
		}
		if err != nil {
			code := http.StatusInternalServerError
			var ae apiError
			switch {
			case errors.As(err, &ae):
				code = ae.code
			case errors.Is(err, store.ErrNotFound):
				code = http.StatusNotFound
			default:
				s.log.Error("api", "path", r.URL.Path, "err", err)
			}
			writeJSON(w, code, map[string]string{"error": err.Error()})
			return
		}
		if out == nil {
			out = map[string]bool{"ok": true}
		}
		writeJSON(w, http.StatusOK, out)
	})
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, bad(errors.New("bad id"))
	}
	return id, nil
}

func (s *Server) Routes(mux *http.ServeMux) {
	h := func(pattern string, fn handler) {
		if s.o.Demo && !strings.HasPrefix(pattern, "GET ") && !slices.Contains([]string{"POST /api/v1/sync", "POST /api/v1/device/forget"}, pattern) {
			fn = func(*http.Request, url.Values) (any, error) {
				return nil, apiError{http.StatusForbidden, "This is a read-only demo server."}
			}
		}
		mux.Handle(pattern, s.wrap(fn, true))
	}
	mux.Handle("POST /api/v1/signin", s.wrap(s.signIn, false))
	h("GET /api/v1/device", func(r *http.Request, _ url.Values) (any, error) { return deviceJSON(device(r.Context())), nil })
	h("POST /api/v1/device/forget", func(r *http.Request, _ url.Values) (any, error) {
		return nil, s.st.DeleteDevice(r.Context(), device(r.Context()).ID)
	})
	h("GET /api/v1/devices", func(r *http.Request, _ url.Values) (any, error) {
		if s.o.Demo {
			d := device(r.Context())
			return []map[string]any{{"id": d.ID, "name": d.Name, "last_sync": unix(d.LastSync), "this": true}}, nil
		}
		devs, err := s.st.Devices(r.Context())
		out := []map[string]any{}
		for _, d := range devs {
			out = append(out, map[string]any{"id": d.ID, "name": d.Name, "last_sync": unix(d.LastSync), "this": d.ID == device(r.Context()).ID})
		}
		return out, err
	})
	h("GET /api/v1/sync", s.pull)
	h("POST /api/v1/sync", s.push)

	// Deprecated: follows without checking the address; apps up to 1.1.1 use it. Remove once the app release after 1.1.1 is out.
	h("POST /api/v1/channels", s.followUnchecked)
	h("POST /api/v1/preview", s.preview)
	h("POST /api/v1/follow", s.follow)
	h("GET /api/v1/directory", s.directory)
	h("POST /api/v1/channels/{id}/settings", s.channelSettings)
	h("POST /api/v1/channels/{id}/speed", s.channelSpeed)
	h("POST /api/v1/channels/{id}/check", s.channelCheck)
	h("POST /api/v1/channels/{id}/readback", s.channelReadBack)
	h("DELETE /api/v1/channels/{id}", func(r *http.Request, _ url.Values) (any, error) {
		id, err := pathID(r)
		if err != nil {
			return nil, err
		}
		return nil, s.st.DeleteChannel(r.Context(), id)
	})
	h("POST /api/v1/checkall", func(r *http.Request, _ url.Values) (any, error) { return nil, s.jobs.CheckAll(r.Context()) })
	h("POST /api/v1/opml", s.opmlImport)
	mux.Handle("GET /api/v1/opml", s.wrap(s.opmlExport, true))

	h("GET /api/v1/tags", s.tags)
	h("POST /api/v1/tags/rename", s.tagRename)

	h("GET /api/v1/episodes", s.episodes)
	h("GET /api/v1/episodes/{id}", s.episode)
	h("POST /api/v1/episodes/{id}/action", s.episodeAction)
	h("GET /api/v1/episodes/{id}/stream", s.streamLink)

	h("POST /api/v1/playlists", s.playlistCreate)
	h("POST /api/v1/playlists/move", func(r *http.Request, in url.Values) (any, error) {
		m, err := intake.ParseMove(in)
		if err != nil {
			return nil, bad(err)
		}
		return nil, s.st.MovePlaylist(r.Context(), m.ID, m.Target, m.After)
	})
	h("POST /api/v1/playlists/{id}", s.playlistUpdate)
	h("DELETE /api/v1/playlists/{id}", func(r *http.Request, _ url.Values) (any, error) {
		id, err := pathID(r)
		if err != nil {
			return nil, err
		}
		return nil, s.st.DeletePlaylist(r.Context(), id)
	})
	h("POST /api/v1/playlists/{id}/order", s.playlistOrder)
	h("POST /api/v1/playlists/{id}/reshuffle", func(r *http.Request, _ url.Values) (any, error) {
		id, err := pathID(r)
		if err != nil {
			return nil, err
		}
		return nil, s.st.ReshufflePlaylist(r.Context(), id)
	})
	h("POST /api/v1/playlists/{id}/fix-dates", func(r *http.Request, _ url.Values) (any, error) {
		id, err := pathID(r)
		if err != nil {
			return nil, err
		}
		p, err := s.st.Playlist(r.Context(), id)
		if err != nil {
			return nil, err
		}
		v, err := s.lib.Load(r.Context())
		if err != nil {
			return nil, err
		}
		ids, ferr := v.PlaylistIDs(p)
		if ferr != nil {
			return nil, bad(errors.New(ferr.Msg))
		}
		err = s.jobs.FixDates(r.Context(), p.Name, ids)
		if errors.Is(err, jobs.ErrDatesBusy) {
			return nil, apiError{http.StatusConflict, err.Error()}
		}
		return nil, err
	})
	h("GET /api/v1/filter/check", s.filterCheck)
	h("GET /api/v1/filter/options", s.filterOptions)

	h("GET /api/v1/status", s.status)
	h("POST /api/v1/status/jobs/{id}/cancel", func(r *http.Request, _ url.Values) (any, error) {
		id, err := pathID(r)
		if err != nil {
			return nil, err
		}
		if !s.jobs.Cancel(r.Context(), id) {
			return nil, apiError{http.StatusConflict, "that job is no longer running"}
		}
		return nil, nil
	})
	h("POST /api/v1/status/jobs/{id}/retry", func(r *http.Request, _ url.Values) (any, error) {
		id, err := pathID(r)
		if err != nil {
			return nil, err
		}
		_, err = s.jobs.Retry(r.Context(), id)
		return nil, err
	})

	h("GET /api/v1/settings", func(r *http.Request, _ url.Values) (any, error) {
		set, err := s.st.ServerSettings(r.Context())
		if err != nil {
			return nil, err
		}
		prefs, err := s.st.Prefs(r.Context())
		return map[string]any{"server": set, "server_locked": s.o.LockSettings, "prefs": prefs}, err
	})
	saveServer := func(r *http.Request, in url.Values) (any, error) {
		v, err := intake.ParseServerSettings(in)
		if err != nil {
			return nil, bad(err)
		}
		return nil, s.st.SetServerSettings(r.Context(), v)
	}
	if s.o.LockSettings {
		saveServer = func(*http.Request, url.Values) (any, error) {
			return nil, apiError{http.StatusForbidden, "Server settings are locked on this server."}
		}
	}
	h("POST /api/v1/settings/server", saveServer)
	h("POST /api/v1/settings/prefs", func(r *http.Request, in url.Values) (any, error) {
		v, err := intake.ParsePrefs(in)
		if err != nil {
			return nil, bad(err)
		}
		saved, err := s.st.Prefs(r.Context())
		if err != nil {
			return nil, err
		}
		return nil, s.st.SetPrefs(r.Context(), v.Over(saved))
	})
}

func (s *Server) signIn(r *http.Request, in url.Values) (any, error) {
	req, err := intake.ParseSignIn(in)
	if err != nil {
		return nil, bad(err)
	}
	switch s.gate.Check(intake.Client(r.RemoteAddr, r.Header.Values("X-Forwarded-For")), req.Password) {
	case signin.Throttled:
		return nil, apiError{http.StatusTooManyRequests, "Too many wrong passwords. Wait a minute and try again."}
	case signin.Wrong:
		return nil, apiError{http.StatusUnauthorized, "wrong password"}
	}
	d, tok, err := s.st.CreateDevice(r.Context(), req.Device)
	if err != nil {
		return nil, err
	}
	return map[string]any{"token": tok, "device": deviceJSON(d)}, nil
}

func (s *Server) pull(r *http.Request, in url.Values) (any, error) {
	cursor, _ := strconv.ParseInt(in.Get("cursor"), 10, 64)
	limit, _ := strconv.Atoi(in.Get("limit"))
	if limit <= 0 || limit > 5000 {
		limit = 2000
	}
	ch, err := s.st.Changes(r.Context(), cursor, limit)
	if err != nil {
		return nil, err
	}
	_ = s.st.TouchDevice(r.Context(), device(r.Context()).ID)
	return changesJSON(ch), nil
}

func (s *Server) push(r *http.Request, in url.Values) (any, error) {
	var changes []intake.PlayChangeIn
	if raw := in.Get("changes"); strings.HasPrefix(raw, "[") {
		if err := json.Unmarshal([]byte(raw), &changes); err != nil {
			return nil, bad(errors.New("changes: want a list of play changes"))
		}
	} else {
		for _, c := range in["changes"] {
			var one intake.PlayChangeIn
			if err := json.Unmarshal([]byte(c), &one); err != nil {
				return nil, bad(errors.New("changes: want a list of play changes"))
			}
			changes = append(changes, one)
		}
	}
	pc, err := intake.ParsePlayChanges(changes)
	if err != nil {
		return nil, bad(err)
	}
	if s.o.Demo {
		return map[string]int{"applied": 0}, nil
	}
	if err := s.st.ApplyPlayChanges(r.Context(), pc); err != nil {
		return nil, err
	}
	_ = s.st.TouchDevice(r.Context(), device(r.Context()).ID)
	return map[string]int{"applied": len(pc)}, nil
}

// followUnchecked is the deprecated follow of apps up to 1.1.1.
func (s *Server) followUnchecked(r *http.Request, in url.Values) (any, error) {
	f, err := intake.ParseFollow(in)
	if err != nil {
		return nil, bad(err)
	}
	ch, created, err := s.jobs.FollowUnchecked(r.Context(), store.NewChannel{URL: f.URL, Tags: f.Tags, Username: f.Username, Password: f.Password})
	if err != nil {
		return nil, err
	}
	return map[string]any{"channel": channelJSON(ch), "created": created}, nil
}

// preview reads an address and says what it holds, or why it can't be followed.
func (s *Server) preview(r *http.Request, in url.Values) (any, error) {
	f, err := intake.ParseFollow(in)
	if err != nil {
		return nil, bad(err)
	}
	p, err := s.jobs.Preview(r.Context(), store.NewChannel{URL: f.URL, Username: f.Username, Password: f.Password})
	if err != nil {
		return nil, apiError{http.StatusUnprocessableEntity, err.Error()}
	}
	return map[string]any{"url": p.URL, "kind": p.Kind, "title": p.Title, "author": p.Author, "artwork": p.Artwork, "description": p.Description,
		"episodes": p.Episodes, "media": p.Media, "following": p.Following}, nil
}

// follow checks the address as preview does, then follows it.
func (s *Server) follow(r *http.Request, in url.Values) (any, error) {
	f, err := intake.ParseFollow(in)
	if err != nil {
		return nil, bad(err)
	}
	ch, created, err := s.jobs.Follow(r.Context(), store.NewChannel{URL: f.URL, Title: f.Title, Tags: f.Tags, Username: f.Username, Password: f.Password})
	if errors.Is(err, context.Canceled) {
		return nil, err
	}
	if err != nil {
		return nil, apiError{http.StatusUnprocessableEntity, err.Error()}
	}
	return map[string]any{"channel": channelJSON(ch), "created": created}, nil
}

// directory searches the podcast directory for something to follow.
func (s *Server) directory(r *http.Request, in url.Values) (any, error) {
	term, err := intake.ParseSearch(in)
	if err != nil {
		return nil, bad(err)
	}
	found, err := s.feed.Search(r.Context(), term)
	if err != nil {
		return nil, apiError{http.StatusBadGateway, err.Error()}
	}
	out := []map[string]any{}
	for _, f := range found {
		out = append(out, map[string]any{"title": f.Title, "author": f.Author, "url": f.URL, "artwork": f.Artwork, "episodes": f.Episodes})
	}
	return map[string]any{"results": out}, nil
}

func (s *Server) channelSettings(r *http.Request, in url.Values) (any, error) {
	id, err := pathID(r)
	if err != nil {
		return nil, err
	}
	set, err := intake.ParseChannelSettings(in)
	if err != nil {
		return nil, bad(err)
	}
	return nil, s.st.SetChannelSettings(r.Context(), id, set)
}

func (s *Server) channelSpeed(r *http.Request, in url.Values) (any, error) {
	id, err := pathID(r)
	if err != nil {
		return nil, err
	}
	v, err := intake.ParseChannelSpeed(in)
	if err != nil {
		return nil, bad(err)
	}
	return nil, s.st.SetChannelSpeed(r.Context(), id, v.Speed)
}

func (s *Server) channelCheck(r *http.Request, _ url.Values) (any, error) {
	id, err := pathID(r)
	if err != nil {
		return nil, err
	}
	job, err := s.jobs.CheckNow(r.Context(), id, jobs.ReasonNow)
	return map[string]int64{"job": job}, err
}

func (s *Server) channelReadBack(r *http.Request, in url.Values) (any, error) {
	id, err := pathID(r)
	if err != nil {
		return nil, err
	}
	set, err := s.st.ServerSettings(r.Context())
	if err != nil {
		return nil, err
	}
	rb, err := intake.ParseReadBack(in, set.DeepDepth)
	if err != nil {
		return nil, bad(err)
	}
	job, err := s.jobs.ReadBack(r.Context(), id, rb.Depth)
	return map[string]int64{"job": job}, err
}

func (s *Server) opmlImport(r *http.Request, in url.Values) (any, error) {
	entries, err := feed.ParseOPML(strings.NewReader(in.Get("opml")))
	if err != nil {
		return nil, bad(fmt.Errorf("opml: %w", err))
	}
	added := 0
	for _, e := range entries {
		u, err := intake.NormalizeURL(e.URL)
		if err != nil {
			continue
		}
		_, created, err := s.jobs.FollowUnchecked(r.Context(), store.NewChannel{URL: u, Tags: store.NormalizeTags(store.TagFromName(e.Folder))})
		if err != nil {
			return nil, err
		}
		if created {
			added++
		}
	}
	return map[string]int{"added": added, "found": len(entries)}, nil
}

func (s *Server) opmlExport(r *http.Request, _ url.Values) (any, error) {
	chs, err := s.st.Channels(r.Context())
	if err != nil {
		return nil, err
	}
	var entries []feed.OPMLEntry
	for _, c := range chs {
		entries = append(entries, feed.OPMLEntry{URL: c.URL, Title: c.Name()})
	}
	var b strings.Builder
	if err := feed.WriteOPML(&b, entries); err != nil {
		return nil, err
	}
	return map[string]string{"opml": b.String()}, nil
}

func (s *Server) tags(r *http.Request, _ url.Values) (any, error) {
	tags, err := s.st.Tags(r.Context())
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, t := range tags {
		ids := []int64{}
		for _, c := range t.Channels {
			ids = append(ids, c.ID)
		}
		out = append(out, map[string]any{"name": t.Name, "channels": ids})
	}
	return out, nil
}

func (s *Server) tagRename(r *http.Request, in url.Values) (any, error) {
	tr, err := intake.ParseTagRename(in)
	if err != nil {
		return nil, bad(err)
	}
	v, err := s.lib.Load(r.Context())
	if err != nil {
		return nil, err
	}
	return nil, s.st.RenameTag(r.Context(), tr.From, tr.To, func(f string) string { return filter.RenameTag(f, tr.From, tr.To, v.Env) })
}

func (s *Server) episodes(r *http.Request, in url.Values) (any, error) {
	v, err := s.lib.Load(r.Context())
	if err != nil {
		return nil, err
	}
	f, ferr := v.Parse(in.Get("filter"))
	if ferr != nil {
		return nil, apiError{http.StatusBadRequest, fmt.Sprintf("%s (at %d)", ferr.Msg, ferr.Pos+1)}
	}
	idx := v.Match(f)
	q := intake.ParseEpisodeQuery(url.Values{"sort": {in.Get("sort")}})
	v.Sort(idx, q.Sort, nil, 0)
	cursor, _ := strconv.Atoi(in.Get("cursor"))
	limit, _ := strconv.Atoi(in.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	cursor = min(max(cursor, 0), len(idx))
	end := min(cursor+limit, len(idx))
	ids := []int64{}
	for _, i := range idx[cursor:end] {
		ids = append(ids, v.Episodes[i].ID)
	}
	next := ""
	if end < len(idx) {
		next = strconv.Itoa(end)
	}
	return map[string]any{"ids": ids, "total": len(idx), "cursor": next}, nil
}

func (s *Server) episode(r *http.Request, _ url.Values) (any, error) {
	id, err := pathID(r)
	if err != nil {
		return nil, err
	}
	e, err := s.st.Episode(r.Context(), id)
	if err != nil {
		return nil, err
	}
	p, err := s.st.Play(r.Context(), id)
	return map[string]any{"episode": episodeJSON(e), "play": playJSON(p)}, err
}

func (s *Server) episodeAction(r *http.Request, in url.Values) (any, error) {
	id, err := pathID(r)
	if err != nil {
		return nil, err
	}
	a, err := intake.ParseEpisodeAction(in)
	if err != nil {
		return nil, bad(err)
	}
	ctx := r.Context()
	switch a.Action {
	case "position":
		return nil, s.st.SetPosition(ctx, id, a.Position)
	case "played", "unplayed":
		return nil, s.st.MarkMany(ctx, []int64{id}, store.FieldPlayed, a.Action == "played")
	case "star", "unstar":
		return nil, s.st.MarkMany(ctx, []int64{id}, store.FieldStarred, a.Action == "star")
	}
	e, err := s.st.Episode(ctx, id)
	if err != nil {
		return nil, err
	}
	n, err := s.st.MarkOlderPlayed(ctx, e)
	return map[string]int{"marked": n}, err
}

// streamLink returns the signed downloader engine stream address, resolving it first so errors come back here.
func (s *Server) streamLink(r *http.Request, in url.Values) (any, error) {
	id, err := pathID(r)
	if err != nil {
		return nil, err
	}
	e, err := s.st.Episode(r.Context(), id)
	if err != nil {
		return nil, err
	}
	if !e.Streamed() {
		return map[string]any{"url": e.MediaURL, "joined": false}, nil
	}
	p, err := s.streams.Prepare(r.Context(), e)
	if err != nil {
		return nil, apiError{http.StatusBadGateway, "downloader engine: " + err.Error()}
	}
	// A listing doesn't always give a length; keep the one the engine found, so every device shows it.
	if e.Duration == 0 && p.Duration > 0 {
		if err := s.st.SetExactDetails(r.Context(), id, store.EpisodeDetails{Duration: p.Duration}); err != nil {
			return nil, err
		}
	}
	return map[string]any{"url": s.streams.Link(id, in.Get("download") == "1"), "joined": p.Joined, "download": s.streams.Link(id, true), "size": p.Size,
		"duration": p.Duration}, nil
}

func (s *Server) storedFilter(r *http.Request, display string) (string, error) {
	v, err := s.lib.Load(r.Context())
	if err != nil {
		return "", err
	}
	stored, ferr := filter.ToStored(display, v.Env)
	if ferr != nil {
		return "", apiError{http.StatusBadRequest, fmt.Sprintf("filter: %s (at %d)", ferr.Msg, ferr.Pos+1)}
	}
	return stored, nil
}

func (s *Server) playlistCreate(r *http.Request, in url.Values) (any, error) {
	p, err := intake.ParsePlaylist(in)
	if err != nil {
		return nil, bad(err)
	}
	stored, err := s.storedFilter(r, p.Filter)
	if err != nil {
		return nil, err
	}
	id, err := s.st.CreatePlaylist(r.Context(), store.PlaylistIn{Name: p.Name, Filter: stored, Sort: p.Sort, Autoplay: p.Autoplay, HidePlayed: p.HidePlayed(true)})
	return map[string]int64{"id": id}, err
}

func (s *Server) playlistUpdate(r *http.Request, in url.Values) (any, error) {
	id, err := pathID(r)
	if err != nil {
		return nil, err
	}
	p, err := intake.ParsePlaylist(in)
	if err != nil {
		return nil, bad(err)
	}
	stored, err := s.storedFilter(r, p.Filter)
	if err != nil {
		return nil, err
	}
	saved, err := s.st.Playlist(r.Context(), id)
	if err != nil {
		return nil, err
	}
	upd := store.PlaylistIn{Name: p.Name, Filter: stored, Sort: p.Sort, Autoplay: p.Autoplay, HidePlayed: p.HidePlayed(saved.HidePlayed)}
	return nil, s.st.UpdatePlaylist(r.Context(), id, upd)
}

func (s *Server) playlistOrder(r *http.Request, in url.Values) (any, error) {
	id, err := pathID(r)
	if err != nil {
		return nil, err
	}
	m, err := intake.ParseMove(in)
	if err != nil {
		return nil, bad(err)
	}
	p, err := s.st.Playlist(r.Context(), id)
	if err != nil {
		return nil, err
	}
	v, err := s.lib.Load(r.Context())
	if err != nil {
		return nil, err
	}
	manual, err := s.st.ManualOrder(r.Context(), id)
	if err != nil {
		return nil, err
	}
	p.Sort = "manual"
	idx, ferr := v.Playlist(p, manual)
	if ferr != nil {
		return nil, bad(errors.New(ferr.Msg))
	}
	order := make([]int64, len(idx))
	for i, x := range idx {
		order[i] = v.Episodes[x].ID
	}
	return nil, s.st.PlaceInPlaylist(r.Context(), id, order, m.ID, m.Target, m.After)
}

func (s *Server) filterCheck(r *http.Request, in url.Values) (any, error) {
	v, err := s.lib.Load(r.Context())
	if err != nil {
		return nil, err
	}
	f, ferr := v.Parse(in.Get("filter"))
	if ferr != nil {
		return map[string]any{"error": ferr.Msg, "pos": ferr.Pos}, nil
	}
	return map[string]any{"count": len(v.Match(f))}, nil
}

func (s *Server) filterOptions(r *http.Request, in url.Values) (any, error) {
	v, err := s.lib.Load(r.Context())
	if err != nil {
		return nil, err
	}
	var opts []filter.Option
	switch in.Get("key") {
	case "channel":
		opts = v.Env.Channels
	default:
		return nil, bad(errors.New("key: channel"))
	}
	q := strings.ToLower(in.Get("search"))
	out := []map[string]string{}
	for _, o := range opts {
		if q == "" || strings.Contains(strings.ToLower(o.Label), q) {
			out = append(out, map[string]string{"id": o.ID, "label": o.Label})
		}
	}
	cursor, _ := strconv.Atoi(in.Get("cursor"))
	cursor = min(max(cursor, 0), len(out))
	end := min(cursor+50, len(out))
	next := ""
	if end < len(out) {
		next = strconv.Itoa(end)
	}
	return map[string]any{"options": out[cursor:end], "cursor": next}, nil
}

func (s *Server) status(r *http.Request, _ url.Values) (any, error) {
	ctx := r.Context()
	runs := []map[string]any{}
	for _, x := range s.track.Runs() {
		if x.Kind == store.JobEngine {
			continue
		}
		runs = append(runs, map[string]any{"job": x.JobID, "kind": x.Kind, "label": x.Label, "reason": x.Reason, "channel": x.ChannelID,
			"stage": x.Stage, "done": x.Done, "total": x.Total, "started": x.Started.Unix()})
	}
	streams := []map[string]any{}
	for _, x := range s.track.Streams() {
		streams = append(streams, map[string]any{"id": x.ID, "episode": x.EpisodeID, "title": x.Title, "mode": x.Mode, "client": x.Client,
			"offset": x.Offset, "bytes": x.Bytes, "started": x.Started.Unix()})
	}
	ended := []map[string]any{}
	for _, x := range s.track.EndedStreams() {
		ended = append(ended, map[string]any{"id": x.ID, "episode": x.EpisodeID, "title": x.Title, "mode": x.Mode, "client": x.Client,
			"offset": x.Offset, "bytes": x.Bytes, "started": x.Started.Unix(), "ended": x.Ended.Unix(), "error": x.Err})
	}
	queued := []map[string]any{}
	for _, j := range s.jobs.Queued() {
		if j.Kind == store.JobEngine {
			continue
		}
		queued = append(queued, jobJSON(j))
	}
	hist, err := s.st.History(ctx, false, 100)
	if err != nil {
		return nil, err
	}
	history := []map[string]any{}
	for _, j := range hist {
		if j.Kind == store.JobEngine {
			continue
		}
		history = append(history, jobJSON(j))
	}
	chs, err := s.st.Channels(ctx)
	if err != nil {
		return nil, err
	}
	due := []map[string]any{}
	for _, c := range chs {
		due = append(due, map[string]any{"channel": c.ID, "title": c.Name(), "next_check": c.NextCheck.Unix()})
	}
	set, err := s.st.ServerSettings(ctx)
	if err != nil {
		return nil, err
	}
	held := s.streams.CacheUsage()
	cache := map[string]any{"bytes": held.Bytes, "files": held.Files, "limit_gb": set.CacheGB, "hours": set.CacheHours}
	return map[string]any{"running": runs, "streams": streams, "ended_streams": ended, "queued": queued, "history": history, "due": due, "cache": cache}, nil
}
