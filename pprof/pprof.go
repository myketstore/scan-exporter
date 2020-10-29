package pprof

import (
	"net/http"
	"time"

	// Import and mount pprof
	_ "net/http/pprof"
)

// Server holds pprof server required informations
type Server struct {
	http.Server
}

// New returns a pprof server instance
func New(addr string) (*Server, error) {
	s := Server{}
	s.ReadTimeout = time.Second
	s.Addr = addr
	return &s, nil
}

// Run an independent pprof server
func (p *Server) Run() error {
	return p.ListenAndServe()
}