// Package socket implements the daemon's JSON-RPC surface: newline-delimited
// JSON over a unix socket, one request per connection.
package socket

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// Limits on one connection. The socket is local, but the daemon holds Notion
// access tokens, so a buggy or hostile local process must not be able to pin
// its goroutines or make it allocate without bound.
const (
	// maxRequestBytes caps a single JSON-RPC request line.
	maxRequestBytes = 1 << 20
	// requestDeadline bounds how long one connection may occupy the daemon.
	// Handlers render and upload, so it is generous rather than tight.
	requestDeadline = 5 * time.Minute
)

// Request is the wire format for a single call.
type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response wraps every reply. Exactly one of Result or Error is set.
type Response struct {
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// StatusResult answers the `status` method.
type StatusResult struct {
	Authed    bool   `json:"authed"`
	Workspace string `json:"workspace"`
}

// Handler serves one method. Params are the raw JSON params from the request.
type Handler func(params json.RawMessage) (any, error)

// Server listens on a unix socket and dispatches requests to handlers.
type Server struct {
	path     string
	handlers map[string]Handler

	mu       sync.Mutex
	listener net.Listener
	wg       sync.WaitGroup
}

// NewServer returns a server with the default method set registered.
// Methods beyond `status` are stubs until their milestones land.
func NewServer(path string) *Server {
	s := &Server{path: path, handlers: map[string]Handler{}}
	s.Register("status", s.handleStatus)
	return s
}

// Register installs a handler for a method name, replacing any existing one.
func (s *Server) Register(method string, h Handler) {
	s.handlers[method] = h
}

// Start begins accepting connections. It removes a stale socket file first.
func (s *Server) Start() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	l, err := net.Listen("unix", s.path)
	if err != nil {
		return err
	}
	// net.Listen creates the socket with 0755&^umask. Anything that can
	// connect can drive the daemon's Notion calls with the stored token, so
	// narrow it to the owner. The window between bind and chmod is why the
	// daemon and its UI both run as the same (only) user on the device.
	if err := os.Chmod(s.path, 0o600); err != nil {
		l.Close()
		return fmt.Errorf("restrict socket permissions: %w", err)
	}
	s.mu.Lock()
	s.listener = l
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := l.Accept()
			if err != nil {
				return // listener closed
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.serveConn(conn)
			}()
		}
	}()
	return nil
}

// Close stops the listener and waits for in-flight connections.
func (s *Server) Close() error {
	s.mu.Lock()
	l := s.listener
	s.mu.Unlock()
	var err error
	if l != nil {
		err = l.Close()
	}
	s.wg.Wait()
	if rmErr := os.Remove(s.path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) && err == nil {
		err = rmErr
	}
	return err
}

func (s *Server) serveConn(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(requestDeadline))

	var req Request
	// One request per connection, size-capped: a client that never sends a
	// newline cannot make the daemon buffer without limit.
	line, err := bufio.NewReader(io.LimitReader(conn, maxRequestBytes)).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	resp := Response{}
	if err := json.Unmarshal(line, &req); err != nil {
		resp.Error = "malformed request: " + err.Error()
	} else if h, ok := s.handlers[req.Method]; !ok {
		resp.Error = "unknown method: " + req.Method
	} else if result, err := h(req.Params); err != nil {
		resp.Error = err.Error()
	} else {
		resp.Result = result
	}

	out, err := json.Marshal(resp)
	if err != nil {
		out = []byte(`{"error":"internal: failed to encode response"}`)
	}
	conn.Write(append(out, '\n'))
}

func (s *Server) handleStatus(json.RawMessage) (any, error) {
	// Token storage lands in M2; until then the daemon is never authed.
	return StatusResult{Authed: false, Workspace: ""}, nil
}
