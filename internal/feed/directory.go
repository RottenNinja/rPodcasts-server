package feed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Found is one podcast a directory search turned up.
type Found struct {
	Title    string
	Author   string
	URL      string
	Artwork  string
	Episodes int
}

// Search asks Apple's podcast directory for podcasts matching the words.
func (c *Client) Search(ctx context.Context, term string) ([]Found, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	q := url.Values{"media": {"podcast"}, "entity": {"podcast"}, "limit": {"30"}, "term": {term}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://itunes.apple.com/search?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the podcast directory can't be reached: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the podcast directory answered %s", resp.Status)
	}
	var body struct {
		Results []struct {
			CollectionName string `json:"collectionName"`
			ArtistName     string `json:"artistName"`
			FeedURL        string `json:"feedUrl"`
			ArtworkURL100  string `json:"artworkUrl100"`
			TrackCount     int    `json:"trackCount"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("the podcast directory's answer can't be read: %w", err)
	}
	out := []Found{}
	for _, r := range body.Results {
		if r.FeedURL == "" {
			continue
		}
		out = append(out, Found{Title: r.CollectionName, Author: r.ArtistName, URL: r.FeedURL, Artwork: r.ArtworkURL100, Episodes: r.TrackCount})
	}
	return out, nil
}
