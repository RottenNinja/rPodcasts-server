package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"podcasts/internal/feed"
	"podcasts/internal/filter"
	"podcasts/internal/intake"
	"podcasts/internal/jobs"
	"podcasts/internal/store"
)

type channelCard struct {
	store.Channel
	Unplayed int
	Running  bool
}

func (s *Server) podcastsPage(w http.ResponseWriter, r *http.Request) {
	v, err := s.view(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	tag := r.URL.Query().Get("tag")
	tags := map[string]bool{}
	var cards []channelCard
	for _, c := range v.Channels {
		for _, t := range c.Tags {
			tags[t] = true
		}
		if tag != "" && !c.HasTag(tag) {
			continue
		}
		cards = append(cards, channelCard{Channel: c, Unplayed: v.Unplayed(c.ID), Running: s.track.Running(c.ID)})
	}
	sort.Slice(cards, func(i, j int) bool {
		return strings.ToLower(cards[i].Name()) < strings.ToLower(cards[j].Name())
	})
	keys := func(m map[string]bool) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
		return out
	}
	s.render(w, r, http.StatusOK, "podcasts", page{Title: "Podcasts", Nav: "podcasts", Data: struct {
		Cards []channelCard
		Tags  []string
		Tag   string
		Total int
		Live  bool
	}{cards, keys(tags), tag, len(v.Channels), s.anyRetrieving(cards)}})
}

func (s *Server) tagNames(ctx context.Context) []string {
	tags, _ := s.st.Tags(ctx)
	out := make([]string, len(tags))
	for i, t := range tags {
		out[i] = t.Name
	}
	return out
}

func (s *Server) anyRetrieving(cards []channelCard) bool {
	for _, c := range cards {
		if c.Retrieving || c.Running {
			return true
		}
	}
	return false
}

func (s *Server) addPage(w http.ResponseWriter, r *http.Request) {
	in, err := intake.ParseAddPage(r.URL.Query())
	var found []feed.Found
	if err == nil && !in.Manual && in.Term != "" {
		found, err = s.feed.Search(r.Context(), in.Term)
	}
	p := page{Title: "Add a podcast", Nav: "podcasts", Crumbs: []crumb{{"Podcasts", "/podcasts"}}, Data: struct {
		intake.AddPage
		AllTags []string
		Found   []feed.Found
		Failed  bool
	}{in, s.tagNames(r.Context()), found, err != nil}}
	if err != nil {
		p.Error = err.Error()
	}
	s.render(w, r, http.StatusOK, "add", p)
}

// manualTab is the Add a channel page on its Manual tab, with the address typed so far.
func manualTab(v url.Values) string {
	return "/podcasts/add?" + url.Values{"tab": {"manual"}, "url": {v.Get("url")}, "tag": {v.Get("tags")}}.Encode()
}

// addPreview reads an address and shows what it holds, with the tags to follow it under.
func (s *Server) addPreview(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	if r.Method == http.MethodPost {
		v = r.PostForm
	}
	if v.Get("url") == "" {
		back(w, r, "/podcasts/add", "", nil)
		return
	}
	if s.o.Demo {
		back(w, r, "/podcasts/add", "", errors.New("This is a read-only demo server."))
		return
	}
	in, err := intake.ParsePreviewPage(v)
	if err != nil {
		back(w, r, manualTab(v), "", err)
		return
	}
	p, err := s.jobs.Preview(r.Context(), store.NewChannel{URL: in.URL, Username: in.Username, Password: in.Password})
	if err != nil {
		back(w, r, manualTab(v), "", err)
		return
	}
	s.render(w, r, http.StatusOK, "preview", page{Title: "Add a podcast", Nav: "podcasts", Crumbs: []crumb{{"Podcasts", "/podcasts"}, {"Add a podcast", "/podcasts/add"}},
		Data: struct {
			intake.PreviewPage
			P       jobs.Preview
			Tags    string
			AllTags []string
		}{in, p, strings.Join(in.Tags, " "), s.tagNames(r.Context())}})
}

func (s *Server) addChannel(w http.ResponseWriter, r *http.Request) {
	f, err := intake.ParseFollow(r.PostForm)
	if err != nil {
		back(w, r, manualTab(r.PostForm), "", err)
		return
	}
	ch, created, err := s.jobs.Follow(r.Context(), store.NewChannel{URL: f.URL, Title: f.Title, Tags: f.Tags, Username: f.Username, Password: f.Password})
	if err != nil {
		back(w, r, manualTab(r.PostForm), "", err)
		return
	}
	msg := "Following. The first read is running."
	if !created {
		msg = "You already follow this podcast."
	}
	back(w, r, fmt.Sprintf("/channels/%d", ch.ID), msg, nil)
}

func (s *Server) checkAll(w http.ResponseWriter, r *http.Request) {
	err := s.jobs.CheckAll(r.Context())
	back(w, r, "/podcasts", "Checking every podcast. Follow along on Status.", err)
}

func (s *Server) opmlExport(w http.ResponseWriter, r *http.Request) {
	chs, err := s.st.Channels(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var entries []feed.OPMLEntry
	for _, c := range chs {
		entries = append(entries, feed.OPMLEntry{URL: c.URL, Title: c.Name()})
	}
	w.Header().Set("Content-Type", "text/x-opml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="podcasts.opml"`)
	_ = feed.WriteOPML(w, entries)
}

func (s *Server) opmlImport(w http.ResponseWriter, r *http.Request) {
	file, _, err := r.FormFile("file")
	if err != nil {
		back(w, r, "/podcasts", "", errors.New("choose an OPML file"))
		return
	}
	defer file.Close()
	entries, err := feed.ParseOPML(file)
	if err != nil {
		back(w, r, "/podcasts", "", fmt.Errorf("OPML: %w", err))
		return
	}
	added, skipped := 0, 0
	for _, e := range entries {
		u, err := intake.NormalizeURL(e.URL)
		if err != nil {
			skipped++
			continue
		}
		_, created, err := s.jobs.FollowUnchecked(r.Context(), store.NewChannel{URL: u, Tags: store.NormalizeTags(store.TagFromName(e.Folder))})
		if err != nil {
			back(w, r, "/podcasts", "", err)
			return
		}
		if created {
			added++
		} else {
			skipped++
		}
	}
	back(w, r, "/podcasts", fmt.Sprintf("Imported %d feeds (%d skipped). They are being read now.", added, skipped), nil)
}

func (s *Server) tagsPage(w http.ResponseWriter, r *http.Request) {
	tags, err := s.st.Tags(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.view(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	type tagView struct {
		store.TagInfo
		Playlists []store.Playlist
	}
	var out []tagView
	for _, t := range tags {
		tv := tagView{TagInfo: t}
		for _, p := range v.Playlists {
			f, ferr := filter.Parse(p.Filter, v.Env)
			if ferr != nil {
				continue
			}
			for _, sp := range f.Spans {
				if sp.Kind == "tag" && sp.ID == t.Name {
					tv.Playlists = append(tv.Playlists, p)
					break
				}
			}
		}
		out = append(out, tv)
	}
	s.render(w, r, http.StatusOK, "tags", page{Title: "Tags", Nav: "podcasts", Crumbs: []crumb{{"Podcasts", "/podcasts"}}, Data: out})
}

func (s *Server) tagRename(w http.ResponseWriter, r *http.Request) {
	in, err := intake.ParseTagRename(r.PostForm)
	if err != nil {
		back(w, r, "/tags", "", err)
		return
	}
	v, err := s.view(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	err = s.st.RenameTag(r.Context(), in.From, in.To, func(f string) string { return filter.RenameTag(f, in.From, in.To, v.Env) })
	back(w, r, "/tags", fmt.Sprintf("Renamed #%s to #%s", in.From, in.To), err)
}

// tagPlaylist is Make a playlist: a playlist with the filter #tag.
func (s *Server) tagPlaylist(w http.ResponseWriter, r *http.Request) {
	tags := store.NormalizeTags(r.PostFormValue("tag"))
	if len(tags) != 1 {
		back(w, r, "/tags", "", errors.New("pick one tag"))
		return
	}
	t := tags[0]
	id, err := s.st.CreatePlaylist(r.Context(), store.PlaylistIn{Name: strings.ToUpper(t[:1]) + t[1:], Filter: "#" + t, Sort: "newest", HidePlayed: true})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	back(w, r, fmt.Sprintf("/playlists/%d", id), "Playlist created", nil)
}
