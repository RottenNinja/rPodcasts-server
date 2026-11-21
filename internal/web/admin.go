package web

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	"podcasts/internal/intake"
	"podcasts/internal/status"
	"podcasts/internal/store"
	"podcasts/internal/stream"
)

type upNextLine struct {
	store.Channel
	Every time.Duration
}

type statusData struct {
	Runs         []status.RunView
	Streams      []status.StreamView
	Ended        []status.StreamView
	Queued       []store.Job
	UpNext       []upNextLine
	History      []store.Job
	Failures     bool
	Engine       store.EngineInfo
	EngineJob    *store.Job
	Cache        stream.CacheUsage
	CachePercent int64
	Settings     store.ServerSettings
	Live         bool
	MoreNext     int
}

func (s *Server) statusData(ctx context.Context, failures bool) (statusData, error) {
	d := statusData{Runs: s.track.Runs(), Streams: s.track.Streams(), Ended: s.track.EndedStreams(), Queued: s.jobs.Queued(), Failures: failures}
	d.Live = len(d.Runs) > 0 || len(d.Streams) > 0 || len(d.Queued) > 0
	chs, err := s.st.Channels(ctx)
	if err != nil {
		return d, err
	}
	set, err := s.st.ServerSettings(ctx)
	if err != nil {
		return d, err
	}
	d.Cache, d.Settings = s.streams.CacheUsage(), set
	d.CachePercent = min(100, d.Cache.Bytes*100/(int64(max(set.CacheGB, 1))<<30))
	sort.Slice(chs, func(i, j int) bool { return chs[i].NextCheck.Before(chs[j].NextCheck) })
	for _, c := range chs {
		d.UpNext = append(d.UpNext, upNextLine{Channel: c, Every: c.Effective(set).CheckEvery})
	}
	if len(d.UpNext) > 30 {
		d.MoreNext = len(d.UpNext) - 30
		d.UpNext = d.UpNext[:30]
	}
	if d.History, err = s.st.History(ctx, failures, 200); err != nil {
		return d, err
	}
	if d.Engine, err = s.st.EngineInfo(ctx); err != nil {
		return d, err
	}
	if d.Engine.LastJobID != 0 {
		if j, err := s.st.Job(ctx, d.Engine.LastJobID); err == nil {
			d.EngineJob = &j
		}
	}
	return d, nil
}

func (s *Server) statusPage(w http.ResponseWriter, r *http.Request) {
	d, err := s.statusData(r.Context(), r.URL.Query().Get("failed") == "1")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "status", page{Title: "Status", Nav: "status", Data: d})
}

func (s *Server) statusLive(w http.ResponseWriter, r *http.Request) {
	d, err := s.statusData(r.Context(), r.URL.Query().Get("failed") == "1")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.partial(w, "status", "live", d)
}

func (s *Server) statusBadge(w http.ResponseWriter, r *http.Request) {
	s.partial(w, "status", "badge", page{Busy: s.track.Busy() + len(s.jobs.Queued()), Failed: s.st.LastFailed(r.Context())})
}

func (s *Server) jobCancel(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	var err error
	if !s.jobs.Cancel(r.Context(), id) {
		err = errors.New("that job is no longer running")
	}
	back(w, r, "/status", "Cancelled", err)
}

func (s *Server) jobRetry(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	_, err := s.jobs.Retry(r.Context(), id)
	back(w, r, "/status", "Queued again", err)
}

func (s *Server) streamStop(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	var err error
	if !s.track.StopStream(id) {
		err = errors.New("that stream has ended")
	}
	back(w, r, "/status", "Stream stopped", err)
}

func (s *Server) engineUpdate(w http.ResponseWriter, r *http.Request) {
	go s.jobs.UpdateEngine(context.WithoutCancel(r.Context()), "Update now")
	time.Sleep(300 * time.Millisecond)
	back(w, r, "/status", "Updating the downloader engine", nil)
}

func (s *Server) devicesPage(w http.ResponseWriter, r *http.Request) {
	data := struct {
		Demo    bool
		Devices []store.Device
	}{Demo: s.o.Demo}
	if !s.o.Demo {
		devs, err := s.st.Devices(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		data.Devices = devs
	}
	s.render(w, r, http.StatusOK, "devices", page{Title: "Devices", Nav: "settings", Crumbs: []crumb{{"Settings", "/settings"}}, Data: data})
}

func (s *Server) deviceForget(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	err := s.st.DeleteDevice(r.Context(), id)
	back(w, r, "/devices", "Forgotten. That device must sign in again.", err)
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	prefs, err := s.st.Prefs(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	set, err := s.st.ServerSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "settings", page{Title: "Settings", Nav: "settings", Data: struct {
		P      store.Prefs
		S      store.ServerSettings
		Zones  []string
		Locked bool
	}{prefs, set, zones, s.o.LockSettings}})
}

func (s *Server) savePrefs(w http.ResponseWriter, r *http.Request) {
	p, err := intake.ParsePrefs(r.PostForm)
	var saved store.Prefs
	if err == nil {
		saved, err = s.st.Prefs(r.Context())
	}
	if err == nil {
		err = s.st.SetPrefs(r.Context(), p.Over(saved))
	}
	back(w, r, "/settings", "Saved", err)
}

func (s *Server) saveServer(w http.ResponseWriter, r *http.Request) {
	v, err := intake.ParseServerSettings(r.PostForm)
	if err == nil {
		err = s.st.SetServerSettings(r.Context(), v)
	}
	back(w, r, "/settings#server", "Saved. New defaults apply from each podcast's next read.", err)
}

var zones = func() []string {
	z := []string{"UTC"}
	for _, region := range []string{"Europe", "America", "Asia", "Australia", "Africa", "Pacific", "Atlantic", "Indian"} {
		for _, city := range zoneCities[region] {
			z = append(z, region+"/"+city)
		}
	}
	return z
}()

var zoneCities = map[string][]string{
	"Europe":    {"London", "Dublin", "Lisbon", "Paris", "Berlin", "Madrid", "Rome", "Amsterdam", "Brussels", "Stockholm", "Oslo", "Copenhagen", "Helsinki", "Warsaw", "Prague", "Vienna", "Zurich", "Athens", "Istanbul", "Kyiv", "Moscow"},
	"America":   {"New_York", "Chicago", "Denver", "Phoenix", "Los_Angeles", "Anchorage", "Toronto", "Vancouver", "Halifax", "Mexico_City", "Bogota", "Lima", "Santiago", "Sao_Paulo", "Buenos_Aires"},
	"Asia":      {"Dubai", "Karachi", "Kolkata", "Dhaka", "Bangkok", "Jakarta", "Singapore", "Hong_Kong", "Shanghai", "Taipei", "Seoul", "Tokyo", "Manila", "Tehran", "Jerusalem", "Riyadh"},
	"Australia": {"Perth", "Adelaide", "Brisbane", "Sydney", "Melbourne", "Hobart"},
	"Africa":    {"Cairo", "Johannesburg", "Lagos", "Nairobi", "Casablanca"},
	"Pacific":   {"Auckland", "Honolulu", "Fiji"},
	"Atlantic":  {"Reykjavik", "Azores", "Canary"},
	"Indian":    {"Maldives", "Mauritius"},
}
