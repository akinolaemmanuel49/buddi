package server

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Default timeouts. ReadHeader and Idle keep the process responsive to slow
// clients; Write is bounded by the per request budget of the handler layer.
const (
	DefaultReadHeaderTimeout = 5 * time.Second
	DefaultReadTimeout       = 30 * time.Second
	DefaultWriteTimeout      = 30 * time.Second
	DefaultIdleTimeout       = 60 * time.Second
)

type Server interface {
	Start() error
	Shutdown(ctx context.Context) error
}

type HTTPServer struct {
	server *http.Server
}

func NewHTTPServer(addr string, handler http.Handler) Server {
	return NewHTTPServerWithOptions(addr, handler, Options{})
}

type Options struct {
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	MaxHeaderBytes    int
}

func NewHTTPServerWithOptions(addr string, handler http.Handler, opts Options) Server {
	if opts.ReadHeaderTimeout <= 0 {
		opts.ReadHeaderTimeout = DefaultReadHeaderTimeout
	}

	if opts.ReadTimeout <= 0 {
		opts.ReadTimeout = DefaultReadTimeout
	}

	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = DefaultWriteTimeout
	}

	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}

	if opts.MaxHeaderBytes <= 0 {
		opts.MaxHeaderBytes = 1 << 20
	}

	return &HTTPServer{
		server: &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: opts.ReadHeaderTimeout,
			ReadTimeout:       opts.ReadTimeout,
			WriteTimeout:      opts.WriteTimeout,
			IdleTimeout:       opts.IdleTimeout,
			MaxHeaderBytes:    opts.MaxHeaderBytes,
		},
	}
}

func (s *HTTPServer) Start() error {
	if err := s.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

func (s *HTTPServer) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}
