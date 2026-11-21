package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"podcasts/internal/feed"
	"podcasts/internal/filter"
	"podcasts/internal/intake"
	"podcasts/internal/library"
	"podcasts/internal/store"
)

const pageSize = 100

func (s *Server) episodesPage(w http.ResponseWriter, r *http.Request) {
	q := intake.ParseEpisodeQuery(r.URL.Query())
	v, err := s.view(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	f, ferr := v.Parse(q.Filter)
	var idx []int
	if ferr == nil {
		idx = v.Match(f)
		v.Sort(idx, q.Sort, nil, 0)
	}
	total := len(idx)
	start := q.Page * pageSize
	if start > len(idx) {
		start = len(idx)
	}
	end := min(start+pageSize, len(idx))
	data := struct {
		Q       intake.EpisodeQuery
		Rows    []epRow
		Total   int
		Page    int
		HasNext bool
		Err     *filter.Error
		Sorts   []struct{ Key, Label string }
	}{q, rowsOf(v, idx[start:end], ""), total, q.Page, end < len(idx), ferr, intake.Sorts[:4]}
	if r.Header.Get("HX-Target") == "episode-results" {
		s.partial(w, "episodes", "results", data)
		return
	}
	s.render(w, r, http.StatusOK, "episodes", page{Title: "Episodes", Nav: "episodes", Data: data})
}

func (s *Server) episodesSelect(w http.ResponseWriter, r *http.Request) {
	sel, err := intake.ParseSelection(r.PostForm["id"], r.PostFormValue("action"))
	ret := "/episodes"
	if u, perr := url.Parse(r.PostFormValue("return")); perr == nil && strings.HasPrefix(u.Path, "/") && !strings.HasPrefix(u.Path, "//") {
		ret = u.RequestURI()
	}
	if err != nil {
		back(w, r, ret, "", err)
		return
	}
	ctx := r.Context()
	switch sel.Action {
	case "played", "unplayed":
		err = s.st.MarkMany(ctx, sel.IDs, store.FieldPlayed, sel.Action == "played")
	case "star", "unstar":
		err = s.st.MarkMany(ctx, sel.IDs, store.FieldStarred, sel.Action == "star")
	}
	back(w, r, ret, fmt.Sprintf("Done for %d episodes", len(sel.IDs)), err)
}

type chapter struct {
	Start int    `json:"start"`
	Title string `json:"title"`
}

func (s *Server) episodePage(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	e, err := s.st.Episode(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ch, err := s.st.Channel(r.Context(), e.ChannelID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p, err := s.st.Play(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.view(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ctxName := r.URL.Query().Get("from")
	next := s.upNext(r.Context(), v, id, ctxName, 5)
	start, _ := strconv.Atoi(r.URL.Query().Get("t"))
	art := e.Artwork
	if art == "" {
		art = ch.Artwork
	}
	position := 0
	if p.InProgress() {
		position = p.Position
	}
	self := fmt.Sprintf("/episodes/%d", e.ID)
	if ctxName != "" {
		self += "?from=" + url.QueryEscape(ctxName)
	}
	s.render(w, r, http.StatusOK, "episode", page{Title: e.Title, Nav: "episodes",
		Crumbs: []crumb{{"Podcasts", "/podcasts"}, {ch.Name(), fmt.Sprintf("/channels/%d", ch.ID)}}, Data: struct {
			E           store.Episode
			C           store.Channel
			P           store.Play
			Art         string
			Notes       template.HTML
			Next        []epRow
			Context     string
			Back        string
			Start       int
			Position    int
			Speed       int
			HasChapters bool
		}{e, ch, p, art, Sanitize(e.Notes), next, ctxName, self, start, position, ch.Speed, e.ChaptersURL != "" || len(e.Chapters) > 0}})
}

// upNext lists what plays after id in the playlist it was started from.
func (s *Server) upNext(ctx context.Context, v *library.View, id int64, from string, n int) []epRow {
	order, name, _ := s.contextOrder(ctx, v, from)
	var out []int
	seen := false
	for _, i := range order {
		e := v.Episodes[i]
		if e.ID == id {
			seen = true
			continue
		}
		if seen && !v.Done(i) {
			out = append(out, i)
		}
		if len(out) >= n {
			break
		}
	}
	return rowsOf(v, out, name)
}

// contextOrder is the playlist an episode was started from, in show order; nothing outside a playlist.
func (s *Server) contextOrder(ctx context.Context, v *library.View, from string) (order []int, name string, autoplay bool) {
	if rest, ok := strings.CutPrefix(from, "playlist:"); ok {
		if pid, err := strconv.ParseInt(rest, 10, 64); err == nil {
			if p, err := s.st.Playlist(ctx, pid); err == nil {
				manual, _ := s.st.ManualOrder(ctx, pid)
				if idx, ferr := v.Playlist(p, manual); ferr == nil {
					return idx, from, p.Autoplay
				}
			}
		}
	}
	return nil, "", false
}

func (s *Server) episodeAction(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	a, err := intake.ParseEpisodeAction(r.PostForm)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	msg := ""
	switch a.Action {
	case "position":
		err = s.st.SetPosition(ctx, id, a.Position)
	case "played", "unplayed":
		err, msg = s.st.MarkMany(ctx, []int64{id}, store.FieldPlayed, a.Action == "played"), "Marked "+a.Action
	case "star", "unstar":
		err, msg = s.st.MarkMany(ctx, []int64{id}, store.FieldStarred, a.Action == "star"), map[bool]string{true: "Starred", false: "Unstarred"}[a.Action == "star"]
	case "older-played":
		var e store.Episode
		if e, err = s.st.Episode(ctx, id); err == nil {
			var n int
			n, err = s.st.MarkOlderPlayed(ctx, e)
			msg = fmt.Sprintf("Marked %d older episodes played", n)
		}
	}
	if a.Action == "position" {
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ret := fmt.Sprintf("/episodes/%d", id)
	if u, perr := url.Parse(r.PostFormValue("return")); perr == nil && strings.HasPrefix(u.Path, "/") && !strings.HasPrefix(u.Path, "//") && r.PostFormValue("return") != "" {
		ret = u.RequestURI()
	}
	back(w, r, ret, msg, err)
}

// episodePlay gives the player everything it needs to start an episode, including what comes next.
func (s *Server) episodePlay(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	ctx := r.Context()
	e, err := s.st.Episode(ctx, id)
	if err != nil {
		http.Error(w, "episode not found", http.StatusNotFound)
		return
	}
	ch, err := s.st.Channel(ctx, e.ChannelID)
	if err != nil {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}
	p, err := s.st.Play(ctx, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	v, err := s.view(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	prefs, _ := s.st.Prefs(ctx)
	type nextInfo struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
	}
	out := struct {
		ID          int64     `json:"id"`
		Title       string    `json:"title"`
		Channel     string    `json:"channel"`
		Artwork     string    `json:"artwork"`
		Src         string    `json:"src"`
		Joined      bool      `json:"joined"`
		Media       string    `json:"media"`
		Start       int       `json:"start"`
		Duration    int       `json:"duration"`
		Speed       int       `json:"speed"`
		SkipStart   int       `json:"skip_start"`
		SkipEnd     int       `json:"skip_end"`
		SkipBack    int       `json:"skip_back"`
		SkipForward int       `json:"skip_forward"`
		NextAt      int       `json:"next_at"`
		Chapters    bool      `json:"chapters"`
		Context     string    `json:"context"`
		Next        *nextInfo `json:"next"`
		Autoplay    bool      `json:"autoplay"`
		PageURL     string    `json:"page_url"`
		Error       string    `json:"error,omitempty"`
	}{ID: e.ID, Title: e.Title, Channel: ch.Name(), Artwork: e.Artwork, Media: e.Media, Duration: e.Duration, Speed: ch.Speed,
		SkipStart: ch.SkipStart, SkipEnd: ch.SkipEnd, SkipBack: prefs.SkipBack, SkipForward: prefs.SkipForward, NextAt: prefs.NextAt, Chapters: e.ChaptersURL != "" || len(e.Chapters) > 0, PageURL: e.PageURL}
	if out.Artwork == "" {
		out.Artwork = ch.Artwork
	}
	if out.Speed == 0 {
		out.Speed = 100
	}
	switch {
	case p.Position > 0 && !p.Played:
		out.Start = p.Position
	default:
		out.Start = ch.SkipStart
	}
	if t, err := strconv.Atoi(r.URL.Query().Get("t")); err == nil && t >= 0 {
		out.Start = t
	}
	if !e.Streamed() {
		out.Src = e.MediaURL
	} else {
		p, err := s.streams.Prepare(ctx, e)
		if err != nil {
			out.Error = "downloader engine: " + err.Error()
		}
		out.Src, out.Joined, out.Duration = s.streams.Link(e.ID, false), p.Joined, max(out.Duration, p.Duration)
	}
	order, name, auto := s.contextOrder(ctx, v, r.URL.Query().Get("from"))
	out.Context, out.Autoplay = name, auto
	if i, ok := v.NextAfter(order, id); ok {
		out.Next = &nextInfo{v.Episodes[i].ID, v.Episodes[i].Title}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

type chapterCache struct {
	mu   sync.Mutex
	data map[int64][]chapter
}

func newChapterCache() *chapterCache { return &chapterCache{data: map[int64][]chapter{}} }

// episodeChapters fetches Podcasting 2.0 JSON chapters through the server, so the page's CSP stays strict.
func (s *Server) episodeChapters(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	s.chap.mu.Lock()
	c, ok := s.chap.data[id]
	s.chap.mu.Unlock()
	if !ok {
		e, err := s.st.Episode(r.Context(), id)
		if err == nil && len(e.Chapters) > 0 {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(e.Chapters)
			return
		}
		if err != nil || e.ChaptersURL == "" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
			return
		}
		c, err = fetchChapters(r.Context(), s.feed, e.ChaptersURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		s.chap.mu.Lock()
		if len(s.chap.data) > 500 {
			s.chap.data = map[int64][]chapter{}
		}
		s.chap.data[id] = c
		s.chap.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(c)
}

func fetchChapters(ctx context.Context, fc *feed.Client, u string) ([]chapter, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := fc.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, errors.New("chapters: " + resp.Status)
	}
	var doc struct {
		Chapters []struct {
			StartTime float64 `json:"startTime"`
			Title     string  `json:"title"`
			TOC       *bool   `json:"toc"`
		} `json:"chapters"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&doc); err != nil {
		return nil, err
	}
	var out []chapter
	for _, c := range doc.Chapters {
		if c.TOC != nil && !*c.TOC {
			continue
		}
		out = append(out, chapter{Start: int(c.StartTime), Title: c.Title})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out, nil
}

// filterCheck shows the live match count, or the error and where it is.
func (s *Server) filterCheck(w http.ResponseWriter, r *http.Request) {
	v, err := s.view(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	text := r.URL.Query().Get("filter")
	if text == "" {
		text = r.URL.Query().Get("q")
	}
	f, ferr := v.Parse(text)
	data := struct {
		Count             int
		Err               *filter.Error
		Before, At, After string
	}{Err: ferr}
	if ferr == nil {
		data.Count = len(v.Match(f))
	} else {
		rs := []rune(text)
		p := min(ferr.Pos, len(rs))
		data.Before, data.At = string(rs[:p]), " "
		if p < len(rs) {
			data.At, data.After = string(rs[p:p+1]), string(rs[p+1:])
		}
	}
	s.partial(w, "episodes", "filtercheck", data)
}

// filterOptions is the paged, searchable options list for $channel and # tags.
func (s *Server) filterOptions(w http.ResponseWriter, r *http.Request) {
	v, err := s.view(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	key, q := r.URL.Query().Get("key"), strings.ToLower(r.URL.Query().Get("search"))
	pg, _ := strconv.Atoi(r.URL.Query().Get("page"))
	var opts []string
	switch key {
	case "channel":
		for _, o := range v.Env.Channels {
			opts = append(opts, "$channel = "+filter.Quote(o.Label))
		}
	default:
		seen := map[string]bool{}
		for _, c := range v.Channels {
			for _, t := range c.Tags {
				if !seen[t] {
					seen[t] = true
					opts = append(opts, "#"+t)
				}
			}
		}
		sort.Strings(opts)
	}
	var match []string
	for _, o := range opts {
		if q == "" || strings.Contains(strings.ToLower(o), q) {
			match = append(match, o)
		}
	}
	const per = 20
	start := min(pg*per, len(match))
	end := min(start+per, len(match))
	s.partial(w, "episodes", "options", struct {
		Key     string
		Search  string
		Options []string
		Page    int
		More    bool
	}{key, q, match[start:end], pg, end < len(match)})
}
