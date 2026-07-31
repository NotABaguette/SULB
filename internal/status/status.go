// Package status exposes live link state over HTTP.
package status

import (
	"encoding/json"
	"net"
	"net/http"
	"time"

	"sulb/internal/links"
	"sulb/internal/pick"
)

type Server struct {
	ln      net.Listener
	mux     *http.ServeMux
	links   []*links.Link
	pickers map[string]*pick.Picker
	start   time.Time
}

func New(listen string, ls []*links.Link, pickers map[string]*pick.Picker) (*Server, error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln, mux: http.NewServeMux(), links: ls, pickers: pickers, start: time.Now()}
	s.mux.HandleFunc("/status", s.handleStatus)
	return s, nil
}

func (s *Server) Set(ls []*links.Link, pickers map[string]*pick.Picker) {
	s.links, s.pickers = ls, pickers
}

func (s *Server) Start() {
	go http.Serve(s.ln, s.mux)
}

func (s *Server) Close() error { return s.ln.Close() }

type statusJSON struct {
	Uptime  string            `json:"uptime"`
	Current map[string]string `json:"current"`
	Links   []links.Snapshot  `json:"links"`
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	out := statusJSON{
		Uptime:  time.Since(s.start).Round(time.Second).String(),
		Current: map[string]string{},
		Links:   make([]links.Snapshot, 0, len(s.links)),
	}
	for k, p := range s.pickers {
		if c := p.Current(); c != nil {
			out.Current[k] = c.Name()
		}
	}
	for _, l := range s.links {
		out.Links = append(out.Links, l.Snapshot())
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
