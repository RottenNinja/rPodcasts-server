package stream

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"podcasts/internal/engine"
)

// byteRange reads "bytes=A-" or "bytes=A-B"; end is -1 for an open range, and ok is false for anything else.
func byteRange(h string) (start, end int64, ok bool) {
	v, found := strings.CutPrefix(h, "bytes=")
	a, b, dash := strings.Cut(v, "-")
	if !found || !dash || strings.Contains(v, ",") {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start < 0 {
		return 0, 0, false
	}
	if b == "" {
		return start, -1, true
	}
	end, err = strconv.ParseInt(b, 10, 64)
	return start, end, err == nil && end >= start
}

// part asks the media host for bytes from-to and returns the body with the size of the whole file.
func (s *Server) part(ctx context.Context, f *engine.Format, from, to int64) (io.ReadCloser, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range f.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, to))
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	_, size, _ := strings.Cut(resp.Header.Get("Content-Range"), "/")
	total, err := strconv.ParseInt(size, 10, 64)
	if resp.StatusCode != http.StatusPartialContent || err != nil {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("media host: %s", resp.Status)
	}
	return resp.Body, total, nil
}

// errSink marks a failure of the reader's side, which asking the media host again would not mend.
type errSink struct{ error }

// chunked copies bytes start..end (-1: the last) of f to w in requests of f.Chunk bytes; head gets the resolved end and the file's size before the first byte.
func (s *Server) chunked(ctx context.Context, w io.Writer, f *engine.Format, start, end int64, head func(end, total int64)) error {
	pos, tries, headed := start, 0, false
	for {
		to := pos + f.Chunk - 1
		if end >= 0 && to > end {
			to = end
		}
		body, total, err := s.part(ctx, f, pos, to)
		if err == nil {
			if !headed {
				if end < 0 || end >= total {
					end = total - 1
				}
				head(end, total)
				headed = true
			}
			var n int64
			n, err = io.Copy(sink{w}, body)
			body.Close()
			pos += n
			if pos > end {
				return nil
			}
			if n > 0 {
				tries = 0
			}
			if err == nil && n == 0 {
				err = errors.New("media host: sent nothing")
			}
		}
		if es := (errSink{}); errors.As(err, &es) {
			return es.error
		}
		if err != nil {
			if tries++; tries >= 3 || ctx.Err() != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(tries) * time.Second):
			}
		}
	}
}

type sink struct{ w io.Writer }

func (s sink) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	if err != nil {
		err = errSink{err}
	}
	return n, err
}

// serveChunked answers one request for f, whole or from a byte range, by fetching it in chunks; out is where the body goes.
func (s *Server) serveChunked(ctx context.Context, w http.ResponseWriter, out io.Writer, f *engine.Format, start, end int64, ranged bool) error {
	headed := false
	err := s.chunked(ctx, out, f, start, end, func(end, total int64) {
		headed = true
		h := w.Header()
		h.Set("Content-Type", mimeOf(f))
		h.Set("Accept-Ranges", "bytes")
		h.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		if !ranged {
			w.WriteHeader(http.StatusOK)
			return
		}
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.WriteHeader(http.StatusPartialContent)
	})
	if err != nil && !headed {
		http.Error(w, err.Error(), http.StatusBadGateway)
	}
	return err
}

// listenInner starts the loopback listener ffmpeg reads its inputs from, so that they too are fetched in chunks.
func (s *Server) listenInner() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	s.inner = "http://" + ln.Addr().String() + "/"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{token}", func(w http.ResponseWriter, r *http.Request) {
		f, ok := s.inputs.Load(r.PathValue("token"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		start, end, ranged := byteRange(r.Header.Get("Range"))
		if !ranged {
			start, end = 0, -1
		}
		_ = s.serveChunked(r.Context(), w, w, f.(*engine.Format), start, end, ranged)
	})
	go func() { _ = http.Serve(ln, mux) }()
	return nil
}

// input is the address ffmpeg reads f from: the loopback listener for a host that wants chunks, else the host itself.
func (s *Server) input(f *engine.Format) (addr string, headers []string, done func()) {
	if f.Chunk <= 0 {
		return f.URL, headerArg(f.Headers), func() {}
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	token := hex.EncodeToString(b)
	s.inputs.Store(token, f)
	return s.inner + token, nil, func() { s.inputs.Delete(token) }
}
