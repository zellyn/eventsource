package eventsource

import (
	"net/http"
	"strings"
)

type gripTransport struct {
	http.RoundTripper
	allowCORS bool
	useGzip   bool
	channel   string
}

const gripChannel = "Grip-Channel"

func makeGripTransport(srv *Server) *gripTransport {
	return &gripTransport{
		http.DefaultTransport,
		srv.AllowCORS,
		srv.Gzip,
		"",
	}
}

func (t *gripTransport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	resp, err = t.RoundTripper.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	h := resp.Header

	t.channel = h.Get(gripChannel)

	if t.channel != "" {
		h.Set("Content-Type", "text/event-stream; charset=utf-8")
		h.Set("Cache-Control", "no-cache, no-store, must-revalidate")
		h.Set("Connection", "keep-alive")
		if t.allowCORS {
			h.Set("Access-Control-Allow-Origin", "*")
		}
		useGzip := t.useGzip && strings.Contains(req.Header.Get("Accept-Encoding"), "gzip")
		if useGzip {
			h.Set("Content-Encoding", "gzip")
		}
	}

	return resp, nil
}
