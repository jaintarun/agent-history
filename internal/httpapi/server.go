package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

const shutdownTimeout = 10 * time.Second

// Server is a running loopback HTTP server.
type Server struct {
	httpServer *http.Server
	listener   net.Listener
	token      string
	done       chan error
}

// Start creates the secured handler and starts listening on a loopback address.
func Start(ctx context.Context, bind string, config Config) (*Server, error) {
	if err := validateLoopbackBind(bind); err != nil {
		return nil, err
	}
	if config.Token == "" {
		token, err := NewToken()
		if err != nil {
			return nil, fmt.Errorf("generate URL token: %w", err)
		}
		config.Token = token
	}
	handler, err := NewHandler(config)
	if err != nil {
		return nil, err
	}
	return startHandler(ctx, bind, config.Token, handler)
}

func startHandler(ctx context.Context, bind, token string, handler http.Handler) (*Server, error) {
	if err := validateLoopbackBind(bind); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", bind)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", bind, err)
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !address.IP.IsLoopback() {
		_ = listener.Close()
		return nil, errors.New("HTTP listener did not resolve to a loopback address")
	}
	server := &Server{
		httpServer: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
		listener: listener,
		token:    token,
		done:     make(chan error, 1),
	}
	serveDone := make(chan error, 1)
	go func() {
		err := server.httpServer.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveDone <- err
		close(serveDone)
	}()
	go func() {
		select {
		case <-ctx.Done():
			shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			shutdownErr := server.httpServer.Shutdown(shutdownContext)
			cancel()
			if shutdownErr != nil {
				_ = server.httpServer.Close()
			}
			serveErr := <-serveDone
			if shutdownErr != nil {
				server.done <- shutdownErr
			} else {
				server.done <- serveErr
			}
		case serveErr := <-serveDone:
			server.done <- serveErr
		}
		close(server.done)
	}()
	return server, nil
}

// URL returns the token-prefixed root URL.
func (s *Server) URL() string {
	return "http://" + s.listener.Addr().String() + "/" + s.token + "/"
}

// Done is closed after the listener and in-flight requests stop.
func (s *Server) Done() <-chan error {
	return s.done
}

// Shutdown stops accepting connections and drains in-flight requests.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func validateLoopbackBind(bind string) error {
	host, _, err := net.SplitHostPort(bind)
	if err != nil {
		return fmt.Errorf("invalid HTTP bind %q: %w", bind, err)
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("HTTP bind must use a loopback address")
	}
	return nil
}
