package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	"podcasts/internal/engine"
	"podcasts/internal/store"
)

// source is what an episode is fetched from: the engine's links, or a podcast's own enclosure.
type source struct {
	engine.Resolved
	// encode is the quality ffmpeg re-encodes Video to ("audio" or a height); empty copies the links as they are.
	encode string
}

// Bytes per second of what ffmpeg makes at each quality, to guess its size before it is made.
var encodedRate = map[string]int64{"audio": 12_000, "480": 125_000, "720": 250_000, "1080": 500_000}

// size is the expected size of the file the cache gets, 0 when unknown.
func (s source) size() int64 {
	switch {
	case s.Single != nil:
		return s.Single.Size
	case s.encode == "audio" && s.Video.ACodec == "aac":
		// Copied audio keeps its own rate, which nothing tells ahead.
		return 0
	case s.encode != "":
		return encodedRate[s.encode] * int64(s.Duration)
	case s.Video != nil && s.Audio != nil && s.Video.Size > 0 && s.Audio.Size > 0:
		// Joined output is the two inputs plus a little MP4 framing.
		return s.Video.Size + s.Audio.Size
	}
	return 0
}

// enclosureTTL is how long a probed enclosure is trusted; its redirects can lead to a link that expires.
const enclosureTTL = time.Hour

// enclosure plans a podcast's own video: passed through when it is no larger than quality, otherwise re-encoded to it.
func (s *Server) enclosure(ctx context.Context, e store.Episode, quality string) (source, error) {
	link, err := s.finalURL(ctx, e.MediaURL)
	if err != nil {
		return source{}, err
	}
	p, err := s.probe(ctx, link)
	if err != nil {
		return source{}, err
	}
	ext := strings.TrimPrefix(path.Ext(strings.SplitN(link, "?", 2)[0]), ".")
	if ext == "" || len(ext) > 4 {
		ext = "mp4"
	}
	f := &engine.Format{URL: link, Ext: strings.ToLower(ext), VCodec: p.vcodec, ACodec: p.acodec, Size: e.Size}
	res := engine.Resolved{Duration: max(e.Duration, p.duration), Expires: time.Now().Add(enclosureTTL)}
	height, _ := strconv.Atoi(quality)
	switch {
	case quality == "audio" && p.acodec != "":
		res.Video = f
		return source{Resolved: res, encode: "audio"}, nil
	case height > 0 && p.height > height:
		res.Video = f
		return source{Resolved: res, encode: quality}, nil
	}
	res.Single = f
	return source{Resolved: res}, nil
}

// finalURL follows the enclosure's redirects with the client that refuses private networks, so ffmpeg is never sent anywhere else.
func (s *Server) finalURL(ctx context.Context, link string) (string, error) {
	if err := s.guard.Web(ctx, link); err != nil {
		return "", fmt.Errorf("media link: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("media host: %s", resp.Status)
	}
	final := resp.Request.URL.String()
	if err := s.guard.Web(ctx, final); err != nil {
		return "", fmt.Errorf("media link: %w", err)
	}
	return final, nil
}

type probed struct {
	vcodec, acodec string
	height         int
	duration       int
}

// probe asks ffprobe what an enclosure holds: its codecs, picture height and length.
func (s *Server) probe(ctx context.Context, link string) (probed, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, s.ffprobe, "-v", "error", "-protocol_whitelist", "http,https,tcp,tls,crypto",
		"-show_entries", "stream=codec_type,codec_name,height:format=duration", "-of", "json", "-i", link).Output()
	if err != nil {
		return probed{}, fmt.Errorf("ffprobe: %w", err)
	}
	var doc struct {
		Streams []struct {
			Type   string `json:"codec_type"`
			Codec  string `json:"codec_name"`
			Height int    `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return probed{}, fmt.Errorf("ffprobe: %w", err)
	}
	var p probed
	for _, st := range doc.Streams {
		switch {
		case st.Type == "video" && p.vcodec == "" && st.Codec != "mjpeg" && st.Codec != "png":
			p.vcodec, p.height = st.Codec, st.Height
		case st.Type == "audio" && p.acodec == "":
			p.acodec = st.Codec
		}
	}
	if d, err := strconv.ParseFloat(doc.Format.Duration, 64); err == nil {
		p.duration = int(d)
	}
	return p, nil
}

// encodeArgs are ffmpeg's mapping and codecs for re-encoding one input to quality; AAC audio is kept as it is.
func encodeArgs(quality, acodec string) []string {
	audio := []string{"-c:a", "aac", "-b:a", "96k"}
	if acodec == "aac" {
		audio = []string{"-c:a", "copy"}
	}
	if quality == "audio" {
		return append([]string{"-map", "0:a:0", "-vn"}, audio...)
	}
	return append([]string{"-map", "0:v:0", "-map", "0:a:0?", "-vf", "scale=-2:" + quality, "-c:v", "libx264", "-preset", "veryfast", "-crf", "26",
		"-pix_fmt", "yuv420p"}, audio...)
}
