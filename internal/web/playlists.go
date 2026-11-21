package web

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"podcasts/internal/filter"
	"podcasts/internal/intake"
	"podcasts/internal/library"
	"podcasts/internal/store"
)

// epRow is one episode line in any list.
type epRow struct {
	ID        int64
	Title     string
	ChannelID int64
	Channel   string
	Artwork   string
	Published time.Time
	Duration  int
	Media     string
	Season    int
	Number    int
	Type      string
	Position  int
	Played    bool
	Starred   bool
	Context   string
}

func (r epRow) Progress() int {
	if r.Duration <= 0 || r.Position <= 0 {
		return 0
	}
	return min(100, r.Position*100/r.Duration)
}

func (r epRow) Left() int { return max(r.Duration-r.Position, 0) }

func rowsOf(v *library.View, idx []int, ctx string) []epRow {
	out := make([]epRow, 0, len(idx))
	for _, i := range idx {
		e := v.Episodes[i]
		ch := v.Channels[e.ChannelID]
		p := v.Plays[e.ID]
		art := e.Artwork
		if art == "" {
			art = ch.Artwork
		}
		out = append(out, epRow{ID: e.ID, Title: e.Title, ChannelID: e.ChannelID, Channel: ch.Name(), Artwork: art, Published: e.Published,
			Duration: e.Duration, Media: e.Media, Season: e.Season, Number: e.Number, Type: e.Type, Position: p.Position, Played: p.Played,
			Starred: p.Starred, Context: ctx})
	}
	return out
}

type playlistLine struct {
	store.Playlist
	NotDone int
	Broken  bool
}

func (s *Server) playlistsPage(w http.ResponseWriter, r *http.Request) {
	v, err := s.view(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var lines []playlistLine
	for _, p := range v.Playlists {
		idx, ferr := v.Playlist(p, nil)
		lines = append(lines, playlistLine{Playlist: p, NotDone: v.NotDone(idx), Broken: ferr != nil})
	}
	s.render(w, r, http.StatusOK, "playlists", page{Title: "Playlists", Nav: "playlists", Data: struct {
		Playlists []playlistLine
		Empty     bool
	}{lines, len(v.Channels) == 0}})
}

func (s *Server) playlistNew(w http.ResponseWriter, r *http.Request) {
	in := store.Playlist{Name: "", Sort: "newest", HidePlayed: true, Filter: r.URL.Query().Get("filter")}
	s.render(w, r, http.StatusOK, "playlist_edit", page{Title: "New playlist", Nav: "playlists", Crumbs: []crumb{{"Playlists", "/"}}, Data: s.editData(in, in.Filter)})
}

func (s *Server) editData(p store.Playlist, display string) any {
	return struct {
		P      store.Playlist
		Filter string
		Sorts  []struct{ Key, Label string }
	}{p, display, intake.Sorts}
}

// checkFilter turns the typed filter into its stored form.
func (s *Server) checkFilter(v *library.View, display string) (string, error) {
	stored, ferr := filter.ToStored(display, v.Env)
	if ferr != nil {
		return "", fmt.Errorf("filter: %s at position %d", ferr.Msg, ferr.Pos+1)
	}
	return stored, nil
}

func (s *Server) playlistCreate(w http.ResponseWriter, r *http.Request) {
	in, err := intake.ParsePlaylist(r.PostForm)
	v, verr := s.view(r.Context())
	if verr != nil {
		s.fail(w, r, verr)
		return
	}
	stored := ""
	if err == nil {
		stored, err = s.checkFilter(v, in.Filter)
	}
	if err != nil {
		p := store.Playlist{Name: in.Name, Sort: in.Sort, Autoplay: in.Autoplay, HidePlayed: in.HidePlayed(true)}
		s.render(w, r, http.StatusUnprocessableEntity, "playlist_edit", page{Title: "New playlist", Nav: "playlists", Error: err.Error(), Data: s.editData(p, in.Filter)})
		return
	}
	id, err := s.st.CreatePlaylist(r.Context(), store.PlaylistIn{Name: in.Name, Filter: stored, Sort: in.Sort, Autoplay: in.Autoplay, HidePlayed: in.HidePlayed(true)})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	back(w, r, fmt.Sprintf("/playlists/%d", id), "Playlist created", nil)
}

func (s *Server) playlistPage(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	p, err := s.st.Playlist(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.view(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	manual, err := s.st.ManualOrder(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	idx, ferr := v.Playlist(p, manual)
	start, hasStart := v.StartOf(idx)
	var startID int64
	if hasStart {
		startID = v.Episodes[start].ID
	}
	ctxName := fmt.Sprintf("playlist:%d", id)
	notDone := v.NotDone(idx)
	q := r.URL.Query()
	// A playlist that hides played episodes still holds them; only the list leaves them out.
	shown, hidden := idx, 0
	if p.HidePlayed && q.Get("played") == "" {
		shown = make([]int, 0, notDone)
		for _, i := range idx {
			if !v.Done(i) {
				shown = append(shown, i)
			}
		}
		hidden = len(idx) - len(shown)
	}
	rows := rowsOf(v, shown, ctxName)
	listed := len(rows)
	if len(rows) > 500 && q.Get("all") == "" {
		rows = rows[:500]
	}
	errText := ""
	if ferr != nil {
		errText = "This playlist's filter no longer reads: " + ferr.Msg + ". Edit it to fix."
	}
	s.render(w, r, http.StatusOK, "playlist", page{Title: p.Name, Nav: "playlists", Crumbs: []crumb{{"Playlists", "/"}}, Error: errText, Data: struct {
		P       store.Playlist
		Filter  string
		Rows    []epRow
		Total   int
		NotDone int
		StartID int64
		Context string
		Manual  bool
		Shuffle bool
		Showing int
		Listed  int
		Hidden  int
		Played  bool
	}{p, v.Display(p.Filter), rows, len(idx), notDone, startID, ctxName, p.Sort == "manual", p.Sort == "shuffle", len(rows), listed, hidden,
		p.HidePlayed && hidden == 0 && notDone < len(idx)}})
}

func (s *Server) playlistEdit(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	p, err := s.st.Playlist(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.view(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "playlist_edit", page{Title: "Edit " + p.Name, Nav: "playlists",
		Crumbs: []crumb{{"Playlists", "/"}, {p.Name, fmt.Sprintf("/playlists/%d", id)}}, Data: s.editData(p, v.Display(p.Filter))})
}

func (s *Server) playlistUpdate(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	p, err := s.st.Playlist(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	in, err := intake.ParsePlaylist(r.PostForm)
	v, verr := s.view(r.Context())
	if verr != nil {
		s.fail(w, r, verr)
		return
	}
	stored := ""
	if err == nil {
		stored, err = s.checkFilter(v, in.Filter)
	}
	if err != nil {
		p.Name, p.Sort, p.Autoplay, p.HidePlayed = in.Name, in.Sort, in.Autoplay, in.HidePlayed(p.HidePlayed)
		s.render(w, r, http.StatusUnprocessableEntity, "playlist_edit", page{Title: "Edit " + p.Name, Nav: "playlists", Error: err.Error(), Data: s.editData(p, in.Filter)})
		return
	}
	upd := store.PlaylistIn{Name: in.Name, Filter: stored, Sort: in.Sort, Autoplay: in.Autoplay, HidePlayed: in.HidePlayed(p.HidePlayed)}
	if err := s.st.UpdatePlaylist(r.Context(), id, upd); err != nil {
		s.fail(w, r, err)
		return
	}
	back(w, r, fmt.Sprintf("/playlists/%d", id), "Saved", nil)
}

func (s *Server) playlistDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	err := s.st.DeletePlaylist(r.Context(), id)
	back(w, r, "/", "Playlist deleted", err)
}

func (s *Server) playlistMark(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	p, err := s.st.Playlist(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.view(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ids, ferr := v.PlaylistIDs(p)
	if ferr != nil {
		back(w, r, fmt.Sprintf("/playlists/%d", id), "", errors.New(ferr.Msg))
		return
	}
	played := r.PostFormValue("value") != "unplayed"
	err = s.st.MarkMany(r.Context(), ids, store.FieldPlayed, played)
	back(w, r, fmt.Sprintf("/playlists/%d", id), fmt.Sprintf("Marked %d episodes", len(ids)), err)
}

func (s *Server) playlistFixDates(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	p, err := s.st.Playlist(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.view(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ids, ferr := v.PlaylistIDs(p)
	if ferr != nil {
		back(w, r, fmt.Sprintf("/playlists/%d", id), "", errors.New(ferr.Msg))
		return
	}
	err = s.jobs.FixDates(r.Context(), p.Name, ids)
	back(w, r, fmt.Sprintf("/playlists/%d", id), "Fixing dates; progress is on the Status page", err)
}

func (s *Server) playlistMove(w http.ResponseWriter, r *http.Request) {
	m, err := intake.ParseMove(r.PostForm)
	if err == nil {
		err = s.st.MovePlaylist(r.Context(), m.ID, m.Target, m.After)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) playlistReshuffle(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	err := s.st.ReshufflePlaylist(r.Context(), id)
	back(w, r, fmt.Sprintf("/playlists/%d", id), "Reshuffled", err)
}

// playlistOrder places one episode before or after another in the playlist's own order.
func (s *Server) playlistOrder(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	m, err := intake.ParseMove(r.PostForm)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.placeInPlaylist(r, id, m); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) placeInPlaylist(r *http.Request, id int64, m intake.Move) error {
	p, err := s.st.Playlist(r.Context(), id)
	if err != nil {
		return err
	}
	v, err := s.view(r.Context())
	if err != nil {
		return err
	}
	manual, err := s.st.ManualOrder(r.Context(), id)
	if err != nil {
		return err
	}
	p.Sort = "manual"
	idx, ferr := v.Playlist(p, manual)
	if ferr != nil {
		return errors.New(ferr.Msg)
	}
	order := make([]int64, len(idx))
	for i, x := range idx {
		order[i] = v.Episodes[x].ID
	}
	return s.st.PlaceInPlaylist(r.Context(), id, order, m.ID, m.Target, m.After)
}
