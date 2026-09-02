package socket

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func startTestServer(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bridge.sock")
	srv := NewServer(path)
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	return path
}

func call(t *testing.T, path, reqLine string) Response {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(reqLine + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", line, err)
	}
	return resp
}

func TestStatus(t *testing.T) {
	path := startTestServer(t)
	resp := call(t, path, `{"method":"status","params":{}}`)
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	result, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatal(err)
	}
	var status StatusResult
	if err := json.Unmarshal(result, &status); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if status.Authed {
		t.Fatal("fresh daemon must not report authed")
	}
}

func TestUnknownMethod(t *testing.T) {
	path := startTestServer(t)
	resp := call(t, path, `{"method":"nope"}`)
	if resp.Error == "" {
		t.Fatal("expected an error for unknown method")
	}
}

func TestMalformedRequest(t *testing.T) {
	path := startTestServer(t)
	resp := call(t, path, `{not json`)
	if resp.Error == "" {
		t.Fatal("expected an error for malformed request")
	}
}

// The daemon holds Notion access tokens and will use them on behalf of
// anything that can talk to this socket, so the socket file must not be
// world-writable.
func TestSocketIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.sock")
	s := NewServer(path)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %04o, want 0600", fi.Mode().Perm())
	}
}

// A client that opens a connection and sends an unterminated flood must not
// be able to make the daemon buffer without bound.
func TestOversizedRequestIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.sock")
	s := NewServer(path)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// No newline, far more than the cap: the read stops at maxRequestBytes,
	// so the server answers and hangs up instead of buffering forever.
	go func() {
		chunk := bytes.Repeat([]byte("a"), 1<<16)
		for range (maxRequestBytes / len(chunk)) + 4 {
			if _, err := conn.Write(chunk); err != nil {
				return
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		io.Copy(io.Discard, conn) // returns once the server hangs up
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("server kept reading past the request cap")
	}
}
