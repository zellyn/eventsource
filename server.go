package eventsource

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

type subscription struct {
	channel     string
	lastEventId string
	out         chan Event
}

type outbound struct {
	channels []string
	event    Event
}
type registration struct {
	channel    string
	repository Repository
}

type Server struct {
	AllowCORS         bool        // Enable all handlers to be accessible from any origin
	ReplayAll         bool        // Replay repository even if there's no Last-Event-Id specified
	BufferSize        int         // How many messages do we let the client get behind before disconnecting
	Gzip              bool        // Enable compression if client can accept it
	Logger            *log.Logger // Logger is a logger that, when set, will be used for logging debug messages
	registrations     chan *registration
	pub               chan *outbound
	subs              chan *subscription
	unregister        chan *subscription
	quit              chan bool
	defaultRepository Repository // A default repository to use if the registration does not have a repository
}

// Create a new Server ready for handler creation and publishing events
func NewServer() *Server {
	srv := &Server{
		registrations: make(chan *registration),
		pub:           make(chan *outbound),
		subs:          make(chan *subscription),
		unregister:    make(chan *subscription, 2),
		quit:          make(chan bool),
		BufferSize:    128,
	}
	go srv.run()
	return srv
}

// Stop handling publishing
func (srv *Server) Close() {
	srv.quit <- true
}

type EventFn func() (Event, error)

func (srv *Server) Handler(channel string) http.HandlerFunc {
	return srv.HandlerWithInitialEvent(channel, nil)
}

// TODO
func (srv *Server) ProxyingHandler(target *url.URL) http.HandlerFunc {
	proxy := httputil.NewSingleHostReverseProxy(target)

	transport := makeGripTransport(srv)

	proxy.Transport = transport

	return func(w http.ResponseWriter, req *http.Request) {
		proxy.ServeHTTP(w, req)

		// Check to see if the Grip-Channel header was specified.
		// If so, hold the connection
		if transport.channel == "" {
			return
		}

		sub := &subscription{
			channel:     transport.channel,
			lastEventId: req.Header.Get("Last-Event-ID"),
			out:         make(chan Event, srv.BufferSize),
		}
		srv.subs <- sub
		flusher := w.(http.Flusher)
		notifier := w.(http.CloseNotifier)
		flusher.Flush()
		useGzip := srv.Gzip && strings.Contains(req.Header.Get("Accept-Encoding"), "gzip")
		enc := NewEncoder(w, useGzip)

		for {
			select {
			case <-notifier.CloseNotify():
				srv.unregister <- sub
				return
			case ev, ok := <-sub.out:
				if !ok {
					return
				}
				if err := enc.Encode(ev); err != nil {
					srv.unregister <- sub
					if srv.Logger != nil {
						srv.Logger.Println(err)
					}
					return
				}
				flusher.Flush()
			}
		}
	}
}

// Create a new handler for serving a specified channel and sending an initial event
func (srv *Server) HandlerWithInitialEvent(channel string, eventFn *EventFn) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/event-stream; charset=utf-8")
		h.Set("Cache-Control", "no-cache, no-store, must-revalidate")
		h.Set("Connection", "keep-alive")
		if srv.AllowCORS {
			h.Set("Access-Control-Allow-Origin", "*")
		}
		useGzip := srv.Gzip && strings.Contains(req.Header.Get("Accept-Encoding"), "gzip")
		if useGzip {
			h.Set("Content-Encoding", "gzip")
		}
		w.WriteHeader(http.StatusOK)

		sub := &subscription{
			channel:     channel,
			lastEventId: req.Header.Get("Last-Event-ID"),
			out:         make(chan Event, srv.BufferSize),
		}
		srv.subs <- sub
		flusher := w.(http.Flusher)
		notifier := w.(http.CloseNotifier)
		flusher.Flush()
		enc := NewEncoder(w, useGzip)

		if eventFn != nil {
			initialEvt, err := (*eventFn)()
			if err != nil {
				if srv.Logger != nil {
					srv.Logger.Println(err)
				}
			} else {
				err := enc.Encode(initialEvt)
				if err != nil {
					if srv.Logger != nil {
						srv.Logger.Println(err)
					}
				} else {
					flusher.Flush()
				}
			}
		}

		for {
			select {
			case <-notifier.CloseNotify():
				srv.unregister <- sub
				return
			case ev, ok := <-sub.out:
				if !ok {
					return
				}
				if err := enc.Encode(ev); err != nil {
					srv.unregister <- sub
					if srv.Logger != nil {
						srv.Logger.Println(err)
					}
					return
				}
				flusher.Flush()
			}
		}
	}
}

// Register the repository to be used for the specified channel
func (srv *Server) Register(channel string, repo Repository) {
	srv.registrations <- &registration{
		channel:    channel,
		repository: repo,
	}
}

// RegisterDefaultRepository registers a repository to be used for all channels
// by default if no per-channel repository has been registered
func (src *Server) RegisterDefaultRepository(repo Repository) {
	src.defaultRepository = repo
}

// Publish an event with the specified id to one or more channels
func (srv *Server) Publish(channels []string, ev Event) {
	srv.pub <- &outbound{
		channels: channels,
		event:    ev,
	}
}

func replay(repo Repository, sub *subscription) {
	for ev := range repo.Replay(sub.channel, sub.lastEventId) {
		sub.out <- ev
	}
}

func (srv *Server) run() {
	subs := make(map[string]map[*subscription]struct{})
	repos := make(map[string]Repository)
	for {
		select {
		case reg := <-srv.registrations:
			if reg.repository != nil {
				repos[reg.channel] = reg.repository
			}
		case sub := <-srv.unregister:
			delete(subs[sub.channel], sub)
		case pub := <-srv.pub:
			for _, c := range pub.channels {
				for s := range subs[c] {
					select {
					case s.out <- pub.event:
					default:
						srv.unregister <- s
						close(s.out)
					}

				}
			}
		case sub := <-srv.subs:
			if _, ok := subs[sub.channel]; !ok {
				subs[sub.channel] = make(map[*subscription]struct{})
			}
			subs[sub.channel][sub] = struct{}{}
			if srv.ReplayAll || len(sub.lastEventId) > 0 {
				repo, ok := repos[sub.channel]
				if ok {
					go replay(repo, sub)
				} else if srv.defaultRepository != nil {
					go replay(srv.defaultRepository, sub)
				}
			}
		case <-srv.quit:
			for _, sub := range subs {
				for s := range sub {
					close(s.out)
				}
			}
			return
		}
	}
}
