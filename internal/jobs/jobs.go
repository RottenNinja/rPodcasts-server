// Package jobs runs the scheduler and refresh workers: reading channels, pruning and the downloader engine's daily update.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"podcasts/internal/engine"
	"podcasts/internal/feed"
	"podcasts/internal/status"
	"podcasts/internal/store"
)

type Manager struct {
	st     *store.Store
	feed   *feed.Client
	engine engine.Engine
	track  *status.Tracker
	log    *slog.Logger

	mu      sync.Mutex
	queue   []store.Job
	running map[int64]bool
	wake    chan struct{}

	engineMu sync.Mutex
	datesMu  sync.Mutex

	previewMu sync.Mutex
	previews  map[previewKey]previewed
}

func New(st *store.Store, fc *feed.Client, eng engine.Engine, track *status.Tracker, log *slog.Logger) *Manager {
	return &Manager{st: st, feed: fc, engine: eng, track: track, log: log, running: map[int64]bool{}, wake: make(chan struct{}, 1),
		previews: map[previewKey]previewed{}}
}

// Reasons shown in the history.
const (
	ReasonDue     = "due"
	ReasonNow     = "Check now"
	ReasonDeep    = "Read further back"
	ReasonAdded   = "added"
	ReasonRetry   = "Retry"
	ReasonDaily   = "daily"
	ReasonMissing = "missing"
	ReasonDates   = "Fix dates"
)

func (m *Manager) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Run schedules due channels and starts queued jobs until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	if err := m.st.AbandonRunning(ctx); err != nil {
		m.log.Error("abandon running jobs", "err", err)
	}
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	var lastPrune time.Time
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		m.enqueueDue(ctx)
		if time.Since(lastPrune) > time.Hour {
			lastPrune = time.Now()
			if set, err := m.st.ServerSettings(ctx); err == nil {
				_ = m.st.PruneHistory(ctx, time.Duration(set.HistoryDays)*24*time.Hour)
			}
		}
		m.dispatch(ctx, &wg)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-m.wake:
		}
	}
}

func (m *Manager) enqueueDue(ctx context.Context) {
	ids, err := m.st.DueChannels(ctx, time.Now())
	if err != nil {
		m.log.Error("due channels", "err", err)
		return
	}
	for _, id := range ids {
		if _, err := m.enqueue(ctx, store.JobRefresh, id, ReasonDue, 0); err != nil {
			m.log.Error("enqueue", "channel", id, "err", err)
		}
	}
}

func (m *Manager) dispatch(ctx context.Context, wg *sync.WaitGroup) {
	set, err := m.st.ServerSettings(ctx)
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for len(m.running) < set.Refreshes {
		idx := -1
		for i, j := range m.queue {
			if !m.running[j.ChannelID] {
				idx = i
				break
			}
		}
		if idx < 0 {
			return
		}
		j := m.queue[idx]
		m.queue = append(m.queue[:idx], m.queue[idx+1:]...)
		m.running[j.ChannelID] = true
		wg.Go(func() {
			m.execute(ctx, j)
			m.mu.Lock()
			delete(m.running, j.ChannelID)
			m.mu.Unlock()
			m.poke()
		})
	}
}

// enqueue adds a job unless the same channel already waits or runs; it returns the job id either way.
func (m *Manager) enqueue(ctx context.Context, kind string, channelID int64, reason string, depth int) (int64, error) {
	m.mu.Lock()
	for _, j := range m.queue {
		if j.ChannelID == channelID && j.Kind == kind {
			m.mu.Unlock()
			return j.ID, nil
		}
	}
	if m.running[channelID] && kind == store.JobRefresh && reason == ReasonDue {
		m.mu.Unlock()
		return 0, nil
	}
	m.mu.Unlock()
	ch, err := m.st.Channel(ctx, channelID)
	if err != nil {
		return 0, err
	}
	j := store.Job{Kind: kind, ChannelID: channelID, Label: ch.Name(), Reason: reason, Depth: depth, State: store.JobQueued, Queued: time.Now()}
	if j.ID, err = m.st.InsertJob(ctx, j); err != nil {
		return 0, err
	}
	if reason == ReasonDue {
		eff := ch.Effective(m.settings(ctx))
		_ = m.st.SetNextCheck(ctx, channelID, time.Now().Add(eff.CheckEvery))
	}
	m.mu.Lock()
	m.queue = append(m.queue, j)
	m.mu.Unlock()
	m.poke()
	return j.ID, nil
}

func (m *Manager) settings(ctx context.Context) store.ServerSettings {
	s, err := m.st.ServerSettings(ctx)
	if err != nil {
		return store.DefaultServerSettings()
	}
	return s
}

func (m *Manager) CheckNow(ctx context.Context, channelID int64, reason string) (int64, error) {
	return m.enqueue(ctx, store.JobRefresh, channelID, reason, 0)
}

func (m *Manager) CheckAll(ctx context.Context) error {
	chs, err := m.st.Channels(ctx)
	if err != nil {
		return err
	}
	for _, c := range chs {
		if _, err := m.enqueue(ctx, store.JobRefresh, c.ID, ReasonNow, 0); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) ReadBack(ctx context.Context, channelID int64, depth int) (int64, error) {
	return m.enqueue(ctx, store.JobDeep, channelID, ReasonDeep, depth)
}

// Queued lists the jobs waiting for a free slot, in order.
func (m *Manager) Queued() []store.Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]store.Job(nil), m.queue...)
}

// Cancel stops a running job or drops a waiting one.
func (m *Manager) Cancel(ctx context.Context, jobID int64) bool {
	if m.track.Cancel(jobID) {
		return true
	}
	m.mu.Lock()
	for i, j := range m.queue {
		if j.ID == jobID {
			m.queue = append(m.queue[:i], m.queue[i+1:]...)
			m.mu.Unlock()
			j.State, j.Finished, j.Error = store.JobCancelled, time.Now(), "cancelled before it started"
			_ = m.st.SaveJob(ctx, j)
			return true
		}
	}
	m.mu.Unlock()
	return false
}

func (m *Manager) Retry(ctx context.Context, jobID int64) (int64, error) {
	j, err := m.st.Job(ctx, jobID)
	if err != nil {
		return 0, err
	}
	if j.Kind == store.JobEngine {
		go m.UpdateEngine(context.WithoutCancel(ctx), ReasonRetry)
		return 0, nil
	}
	if j.Kind == store.JobDates {
		return 0, errors.New("press Fix dates on the playlist again")
	}
	return m.enqueue(ctx, j.Kind, j.ChannelID, ReasonRetry, j.Depth)
}

func (m *Manager) execute(parent context.Context, j store.Job) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	run := m.track.Start(j.ID, j.Kind, j.Label, j.Reason, j.ChannelID, cancel)
	defer m.track.Finish(j.ID)
	j.State, j.Started = store.JobRunning, time.Now()
	_ = m.st.SaveJob(parent, j)
	err := m.refresh(ctx, &j, run)
	j.Finished = time.Now()
	switch {
	case errors.Is(err, context.Canceled) && parent.Err() == nil:
		j.State, j.Error = store.JobCancelled, "cancelled"
	case err != nil:
		j.State, j.Error = store.JobFailed, err.Error()
	default:
		j.State = store.JobDone
	}
	if err := m.st.SaveJob(context.WithoutCancel(parent), j); err != nil {
		m.log.Error("save job", "err", err)
	}
}

// refresh reads one channel; failures are stored on the channel as well as in the run's history.
func (m *Manager) refresh(ctx context.Context, j *store.Job, run *status.Run) error {
	ch, err := m.st.Channel(ctx, j.ChannelID)
	if err != nil {
		return err
	}
	set := m.settings(ctx)
	eff := ch.Effective(set)
	limit := eff.FetchCount
	if j.Kind == store.JobDeep {
		limit = max(j.Depth, 1)
	}
	readErr := m.read(ctx, &ch, j, run, limit)
	if errors.Is(readErr, context.Canceled) {
		_ = m.st.SetChecked(context.WithoutCancel(ctx), ch.ID, time.Now(), time.Now().Add(eff.CheckEvery), "")
		return readErr
	}
	if ch.ID != j.ChannelID {
		return readErr
	}
	if readErr == nil && eff.Keep > 0 {
		run.Stage("pruning", 0, 0)
		n, err := m.st.Prune(ctx, ch.ID, eff.Keep)
		if err != nil {
			readErr = err
		}
		j.Pruned = n
	}
	errText := ""
	if readErr != nil {
		errText = readErr.Error()
	}
	if err := m.st.SetChecked(context.WithoutCancel(ctx), ch.ID, time.Now(), time.Now().Add(eff.CheckEvery), errText); err != nil {
		return err
	}
	if readErr == nil && (j.Summary == "" || j.Pruned > 0) {
		j.Summary = summary(j)
	}
	return readErr
}

func summary(j *store.Job) string {
	if j.NotModified && j.Pruned == 0 {
		return "not modified"
	}
	parts := []string{fmt.Sprintf("added %d", j.Added), fmt.Sprintf("updated %d", j.Updated), fmt.Sprintf("pruned %d", j.Pruned)}
	if j.NotModified {
		parts[0] = "not modified"
		parts = []string{parts[0], parts[2]}
	}
	return strings.Join(parts, ", ")
}

// read fetches with 3 attempts and backoff, within 5 minutes per attempt.
func (m *Manager) read(ctx context.Context, ch *store.Channel, j *store.Job, run *status.Run, limit int) error {
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		actx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		err = m.readOnce(actx, ch, j, run, limit)
		cancel()
		if err == nil || ctx.Err() != nil || errors.Is(err, errPermanent) {
			break
		}
		if attempt < 3 {
			run.Stage(fmt.Sprintf("attempt %d failed, waiting", attempt), 0, 0)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * 5 * time.Second):
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var p permanentErr
	if errors.As(err, &p) {
		return p.err
	}
	return err
}

var errPermanent = errors.New("permanent")

type permanentErr struct{ err error }

func (p permanentErr) Error() string        { return p.err.Error() }
func (p permanentErr) Unwrap() error        { return p.err }
func (p permanentErr) Is(target error) bool { return target == errPermanent }

func (m *Manager) readOnce(ctx context.Context, ch *store.Channel, j *store.Job, run *status.Run, limit int) error {
	if ch.Kind == "video" || !feed.IsWeb(ch.URL) {
		return wrap(m.readVideos(ctx, ch, j, run, limit))
	}
	run.Stage("fetching feed", 0, 0)
	url := ch.URL
	etag, lm := ch.ETag, ch.LastModified
	if j.Kind == store.JobDeep || ch.Retrieving {
		etag, lm = "", ""
	}
	for hop := 0; hop < 3; hop++ {
		res, err := m.feed.Fetch(ctx, feed.Request{URL: url, ETag: etag, LastModified: lm, Username: ch.Username, Password: ch.Password, Limit: max(limit, 400)})
		if err != nil {
			if !ch.Retrieving || errors.Is(err, feed.ErrPrivate) || ctx.Err() != nil {
				return wrap(err)
			}
			// A new address that refuses a plain fetch may still be a page of videos the downloader engine can read.
			if verr := m.readVideos(ctx, ch, j, run, limit); verr != nil {
				return wrap(fmt.Errorf("%v; downloader engine: %v", err, verr))
			}
			return nil
		}
		if res.NotModified {
			j.NotModified = true
			j.Summary = "not modified"
			return nil
		}
		if res.FeedLinks != nil || res.Feed == nil {
			if len(res.FeedLinks) > 0 && hop < 2 {
				url, etag, lm = res.FeedLinks[0], "", ""
				run.Stage("following the feed the page links to", 0, 0)
				continue
			}
			return wrap(m.asVideos(ctx, ch, j, run, limit))
		}
		f := res.Feed
		final := res.URL
		if f.NewFeedURL != "" && f.NewFeedURL != final && hop < 2 {
			url, etag, lm = f.NewFeedURL, "", ""
			run.Stage("following itunes:new-feed-url", 0, 0)
			continue
		}
		var items []store.EpisodeIn
		for _, it := range f.Items {
			if it.MediaURL == "" && it.Link == "" {
				continue
			}
			items = append(items, toEpisode(it))
		}
		playable := 0
		for _, it := range items {
			if it.MediaURL != "" {
				playable++
			}
		}
		if playable == 0 {
			return wrap(m.asVideos(ctx, ch, j, run, limit))
		}
		if limit > 0 && len(items) > limit {
			items = items[:limit]
		}
		if merged, err := m.moveTo(ctx, ch, final, j); merged || err != nil {
			return err
		}
		if err := m.st.SetChannelMeta(ctx, ch.ID, store.ChannelMeta{URL: final, Kind: "feed", Title: f.Title, Author: f.Author, Website: f.Website,
			Description: f.Description, Artwork: f.Artwork, ETag: res.ETag, LastModified: res.LastModified}); err != nil {
			return err
		}
		ch.URL, ch.Kind = final, "feed"
		run.Stage("saving episodes", len(items), len(items))
		return m.save(ctx, ch, j, items)
	}
	return permanentErr{errors.New("too many feed hops")}
}

func wrap(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, feed.ErrPrivate) {
		return permanentErr{err}
	}
	return err
}

// moveTo stores a channel's new address, or folds it into the channel that already has that address.
func (m *Manager) moveTo(ctx context.Context, ch *store.Channel, url string, j *store.Job) (bool, error) {
	if url == ch.URL {
		return false, nil
	}
	other, err := m.st.ChannelByURL(ctx, url)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := m.st.MergeChannel(ctx, ch.ID, other.ID); err != nil {
		return true, err
	}
	j.Summary = "same podcast as " + other.Name() + "; merged into it"
	ch.ID = other.ID
	_, err = m.enqueue(context.WithoutCancel(ctx), store.JobRefresh, other.ID, ReasonNow, 0)
	return true, err
}

func toEpisode(it feed.Item) store.EpisodeIn {
	e := store.EpisodeIn{GUID: it.GUID, MediaURL: it.MediaURL, PageURL: it.Link, Title: it.Title, Notes: it.Notes, Published: it.Published,
		DateQuality: store.DateExact, Duration: it.Duration, Media: it.Media, MIME: it.MIME, Size: it.Size, Season: it.Season, Number: it.Number,
		Type: it.Type, Artwork: it.Artwork, ChaptersURL: it.ChaptersURL, TranscriptURL: it.TranscriptURL}
	if e.Media == "" {
		e.Media = "video"
	}
	if e.Published.IsZero() {
		e.DateQuality = store.DateRough
	}
	return e
}

func (m *Manager) save(ctx context.Context, ch *store.Channel, j *store.Job, items []store.EpisodeIn) error {
	res, err := m.st.UpsertEpisodes(ctx, ch.ID, items)
	if err != nil {
		return err
	}
	j.Added, j.Updated = len(res.Added), res.Updated
	if len(res.Added) > 0 {
		if err := m.st.EnsureBuiltins(ctx); err != nil {
			return err
		}
	}
	return nil
}

// asVideos reads an address that isn't a feed, or a feed with nothing to play, with the downloader engine.
func (m *Manager) asVideos(ctx context.Context, ch *store.Channel, j *store.Job, run *status.Run, limit int) error {
	if !ch.Retrieving && ch.Kind == "feed" && j.Kind != store.JobDeep {
		if ep, _, _ := m.st.EpisodeCounts(ctx, ch.ID); ep > 0 {
			return permanentErr{errors.New("the feed has nothing to play")}
		}
	}
	return m.readVideos(ctx, ch, j, run, limit)
}

// newest cuts a listing to limit: a channel page lists each of its tabs up to the limit, one after another.
func newest(entries []engine.Entry, limit int) []engine.Entry {
	if len(entries) <= limit {
		return entries
	}
	dated := !slices.ContainsFunc(entries, func(e engine.Entry) bool { return e.Timestamp.IsZero() })
	if dated {
		entries = slices.Clone(entries)
		slices.SortStableFunc(entries, func(a, b engine.Entry) int { return b.Timestamp.Compare(a.Timestamp) })
	}
	return entries[:limit]
}

func (m *Manager) readVideos(ctx context.Context, ch *store.Channel, j *store.Job, run *status.Run, limit int) error {
	if limit <= 0 {
		limit = 400
	}
	m.EnsureEngine(ctx)
	run.Stage("reading video list", 0, limit)
	info, err := m.engine.List(ctx, ch.URL, limit, func(n int) { run.Stage("reading video list", n, limit) })
	if err != nil {
		return err
	}
	// Ones saved before they were recognised, or that became members-only since, go.
	if err := m.st.RemoveEpisodes(ctx, ch.ID, info.Locked); err != nil {
		return err
	}
	if len(info.Entries) == 0 {
		return permanentErr{errors.New("nothing to play was found at this address")}
	}
	entries := newest(info.Entries, limit)
	// A list doesn't say audio or video, so new episodes start as the channel's newest is, or as the first one turns out to be.
	media, err := m.st.NewestMedia(ctx, ch.ID)
	if err != nil {
		return err
	}
	if media == "" {
		media = "video"
		if v, err := m.engine.Video(ctx, entries[0].URL); err == nil && v.Media == "audio" {
			media = "audio"
		}
	}
	items := make([]store.EpisodeIn, 0, len(entries))
	var prev time.Time
	for i, e := range entries {
		it := store.EpisodeIn{GUID: e.ID, PageURL: e.URL, Title: e.Title, Duration: e.Duration, Media: media, KeepMedia: true, Type: "full", Artwork: e.Thumbnail}
		if it.GUID == "" {
			it.GUID = e.URL
		}
		switch {
		case !e.Timestamp.IsZero():
			it.Published, it.DateQuality = e.Timestamp, store.DateApprox
		case i == 0:
			it.Published, it.DateQuality = time.Now().Truncate(time.Second), store.DateRough
		default:
			it.Published, it.DateQuality = prev.Add(-time.Second), store.DateRough
		}
		prev = it.Published
		items = append(items, it)
	}
	title := info.Title
	if strings.HasSuffix(title, " - Videos") {
		title = strings.TrimSuffix(title, " - Videos")
	}
	if err := m.st.SetChannelMeta(ctx, ch.ID, store.ChannelMeta{URL: ch.URL, Kind: "video", Title: title, Author: info.Author,
		Website: first(info.Website, ch.URL), Artwork: first(info.Artwork, ch.Artwork, entries[0].Thumbnail)}); err != nil {
		return err
	}
	ch.Kind = "video"
	if err := m.save(ctx, ch, j, items); err != nil {
		return err
	}
	return m.exactDates(ctx, ch, run)
}

// exactDates opens the 20 newest videos with only an approximate date, for their exact dates and show notes.
func (m *Manager) exactDates(ctx context.Context, ch *store.Channel, run *status.Run) error {
	eps, err := m.st.ApproxDatedVideos(ctx, ch.ID, 20)
	if err != nil {
		return err
	}
	_, _, err = m.openDates(ctx, eps, run)
	return err
}

// openDates opens each video's page for its exact date, pausing between requests; it returns how many were fixed and how many failed.
func (m *Manager) openDates(ctx context.Context, eps []store.Episode, run *status.Run) (fixed, failed int, err error) {
	for i, e := range eps {
		run.Stage("fetching dates", i, len(eps))
		if e.PageURL == "" {
			continue
		}
		if i > 0 {
			select {
			case <-ctx.Done():
				return fixed, failed, ctx.Err()
			case <-time.After(1500 * time.Millisecond):
			}
		}
		v, err := m.engine.Video(ctx, e.PageURL)
		if err != nil {
			if ctx.Err() != nil {
				return fixed, failed, ctx.Err()
			}
			m.log.Warn("exact date", "episode", e.ID, "err", err)
			failed++
			continue
		}
		if err := m.st.SetExactDetails(ctx, e.ID, store.DetailsOf(v)); err != nil {
			return fixed, failed, err
		}
		fixed++
	}
	run.Stage("fetching dates", len(eps), len(eps))
	return fixed, failed, nil
}

var ErrDatesBusy = errors.New("dates are already being fixed; see Status")

// FixDates opens every video among the episodes that lacks an exact date, as a job of its own on the Status page.
func (m *Manager) FixDates(parent context.Context, playlistName string, episodeIDs []int64) error {
	if !m.datesMu.TryLock() {
		return ErrDatesBusy
	}
	eps, err := m.st.VideosWithoutExactDates(parent, episodeIDs)
	if err != nil {
		m.datesMu.Unlock()
		return err
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	j := store.Job{Kind: store.JobDates, Label: playlistName, Reason: ReasonDates, State: store.JobRunning, Queued: time.Now(), Started: time.Now()}
	if j.ID, err = m.st.InsertJob(ctx, j); err != nil {
		cancel()
		m.datesMu.Unlock()
		return err
	}
	run := m.track.Start(j.ID, j.Kind, j.Label, j.Reason, 0, cancel)
	go func() {
		defer m.datesMu.Unlock()
		defer cancel()
		defer m.track.Finish(j.ID)
		fixed, failed, err := m.openDates(ctx, eps, run)
		j.Finished, j.Updated = time.Now(), fixed
		j.Summary = fmt.Sprintf("checked %d, fixed %d, failed %d", len(eps), fixed, failed)
		switch {
		case errors.Is(err, context.Canceled):
			j.State, j.Error = store.JobCancelled, "cancelled"
		case err != nil:
			j.State, j.Error = store.JobFailed, err.Error()
		default:
			j.State = store.JobDone
		}
		if err := m.st.SaveJob(context.WithoutCancel(ctx), j); err != nil {
			m.log.Error("save job", "err", err)
		}
	}()
	return nil
}

// EnsureEngine installs the downloader engine if it is missing, and runs its self-update on its first use each day.
func (m *Manager) EnsureEngine(ctx context.Context) {
	info, err := m.st.EngineInfo(ctx)
	if err != nil {
		return
	}
	switch {
	case m.engine.Missing():
		m.UpdateEngine(ctx, ReasonMissing)
	case !sameDay(info.LastUpdate, time.Now()):
		m.UpdateEngine(ctx, ReasonDaily)
	}
}

// UpdateEngine installs or updates the downloader engine as a job of its own on the Status page.
func (m *Manager) UpdateEngine(parent context.Context, reason string) {
	if !m.engineMu.TryLock() {
		m.engineMu.Lock()
		m.engineMu.Unlock()
		return
	}
	defer m.engineMu.Unlock()
	if reason == ReasonDaily || reason == ReasonMissing {
		if info, err := m.st.EngineInfo(parent); err == nil && sameDay(info.LastUpdate, time.Now()) && !m.engine.Missing() {
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Minute)
	defer cancel()
	install := m.engine.Missing()
	label := "Downloader engine update"
	if install {
		label = "Downloader engine install"
	}
	j := store.Job{Kind: store.JobEngine, Label: label, Reason: reason, State: store.JobRunning, Queued: time.Now(), Started: time.Now()}
	id, err := m.st.InsertJob(ctx, j)
	if err != nil {
		return
	}
	j.ID = id
	run := m.track.Start(id, j.Kind, j.Label, reason, 0, cancel)
	defer m.track.Finish(id)
	old, uerr := "", error(nil)
	if install {
		run.Stage("downloading the downloader engine", 0, 0)
		uerr = m.engine.Install(ctx)
	} else {
		run.Stage("updating the downloader engine", 0, 0)
		old = m.engine.Version(ctx)
		uerr = m.engine.Update(ctx)
	}
	j.Finished = time.Now()
	ver := m.engine.Version(ctx)
	switch {
	case uerr != nil:
		j.State, j.Error = store.JobFailed, uerr.Error()
	case install:
		j.State, j.Summary = store.JobDone, "installed "+ver
	case ver != old:
		j.State, j.Summary = store.JobDone, fmt.Sprintf("updated %s → %s", old, ver)
	default:
		j.State, j.Summary = store.JobDone, "up to date ("+ver+")"
	}
	_ = m.st.SaveJob(ctx, j)
	_ = m.st.SetEngineInfo(ctx, store.EngineInfo{Version: ver, LastUpdate: time.Now(), LastJobID: id, LastOutcome: first(j.Summary, j.Error)})
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

func first(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// FollowUnchecked adds a channel without reading its address first; the first read shows it as "being retrieved" until done.
func (m *Manager) FollowUnchecked(ctx context.Context, n store.NewChannel) (store.Channel, bool, error) {
	ch, created, err := m.st.AddChannel(ctx, n)
	if err != nil || !created {
		return ch, created, err
	}
	if err := m.st.EnsureBuiltins(ctx); err != nil {
		return ch, created, err
	}
	_, err = m.enqueue(ctx, store.JobRefresh, ch.ID, ReasonAdded, 0)
	return ch, created, err
}
