package web

import (
	"fmt"
	"net/http"

	"podcasts/internal/intake"
	"podcasts/internal/store"
)

func (s *Server) channelPage(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	ch, err := s.st.Channel(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.view(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	set, err := s.st.ServerSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var idx []int
	for i, e := range v.Episodes {
		if e.ChannelID == id {
			idx = append(idx, i)
		}
	}
	total := len(idx)
	if len(idx) > 300 && r.URL.Query().Get("all") == "" {
		idx = idx[:300]
	}
	running := s.track.Running(id)
	for _, q := range s.jobs.Queued() {
		running = running || q.ChannelID == id
	}
	s.render(w, r, http.StatusOK, "channel", page{Title: ch.Name(), Nav: "podcasts", Crumbs: []crumb{{"Podcasts", "/podcasts"}}, Data: struct {
		C        store.Channel
		Eff      store.Effective
		Set      store.ServerSettings
		Total    int
		Unplayed int
		Rows     []epRow
		Running  bool
	}{ch, ch.Effective(set), set, total, v.Unplayed(id), rowsOf(v, idx, ""), running || ch.Retrieving}})
}

func (s *Server) channelSettingsPage(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	ch, err := s.st.Channel(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	set, err := s.st.ServerSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	video, err := s.st.HasVideo(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "channel_settings", page{Title: "Settings · " + ch.Name(), Nav: "podcasts",
		Crumbs: []crumb{{"Podcasts", "/podcasts"}, {ch.Name(), fmt.Sprintf("/channels/%d", id)}}, Data: struct {
			C       store.Channel
			Set     store.ServerSettings
			AllTags []string
			Video   bool
		}{ch, set, s.tagNames(r.Context()), video || ch.Kind == "video"}})
}

func (s *Server) channelSettings(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	in, err := intake.ParseChannelSettings(r.PostForm)
	if err != nil {
		back(w, r, fmt.Sprintf("/channels/%d/settings", id), "", err)
		return
	}
	err = s.st.SetChannelSettings(r.Context(), id, in)
	back(w, r, fmt.Sprintf("/channels/%d", id), "Saved", err)
}

func (s *Server) channelCheck(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	_, err := s.jobs.CheckNow(r.Context(), id, "Check now")
	back(w, r, fmt.Sprintf("/channels/%d", id), "Checking now. Follow along on Status.", err)
}

func (s *Server) channelReadBack(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	set, err := s.st.ServerSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	in, err := intake.ParseReadBack(r.PostForm, set.DeepDepth)
	if err != nil {
		back(w, r, fmt.Sprintf("/channels/%d", id), "", err)
		return
	}
	_, err = s.jobs.ReadBack(r.Context(), id, in.Depth)
	back(w, r, fmt.Sprintf("/channels/%d", id), fmt.Sprintf("Reading %d episodes back. It runs as its own job on Status.", in.Depth), err)
}

func (s *Server) channelMarkPlayed(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	v, err := s.view(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var ids []int64
	for _, e := range v.Episodes {
		if e.ChannelID == id {
			ids = append(ids, e.ID)
		}
	}
	err = s.st.MarkMany(r.Context(), ids, store.FieldPlayed, true)
	back(w, r, fmt.Sprintf("/channels/%d", id), fmt.Sprintf("Marked %d episodes played", len(ids)), err)
}

func (s *Server) channelUnfollow(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	err := s.st.DeleteChannel(r.Context(), id)
	back(w, r, "/podcasts", "Unfollowed", err)
}
