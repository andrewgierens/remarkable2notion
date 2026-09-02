package pair

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartAndPoll(t *testing.T) {
	polls := 0
	var origin string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/pair":
			// The user code in the URL is deliberately not the device code.
			fmt.Fprintf(w, `{"device_code":"abc123","verification_url":%q}`, origin+"/go/usercode9")
		case r.Method == http.MethodGet && r.URL.Path == "/pair/abc123":
			polls++
			if polls == 1 {
				io.WriteString(w, `{"state":"pending"}`)
			} else {
				io.WriteString(w, `{"state":"ok","access_token":"tok","workspace":"Acme"}`)
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	origin = srv.URL

	c := New(srv.URL + "/") // trailing slash must not break URL joins
	qrPath := filepath.Join(t.TempDir(), "qr.png")

	s, err := c.Start(context.Background(), qrPath)
	if err != nil {
		t.Fatal(err)
	}
	if s.DeviceCode != "abc123" || s.QRPNGPath != qrPath {
		t.Errorf("session = %+v", s)
	}
	png, err := os.ReadFile(qrPath)
	if err != nil {
		t.Fatalf("QR not written: %v", err)
	}
	if len(png) < 8 || string(png[1:4]) != "PNG" {
		t.Error("QR file is not a PNG")
	}

	r1, err := c.Poll(context.Background(), "abc123")
	if err != nil || r1.State != StatePending {
		t.Fatalf("first poll = %+v, %v", r1, err)
	}
	r2, err := c.Poll(context.Background(), "abc123")
	if err != nil || r2.State != StateOK || r2.Token != "tok" || r2.Workspace != "Acme" {
		t.Fatalf("second poll = %+v, %v", r2, err)
	}
}

func TestPollExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"state":"expired"}`)
	}))
	defer srv.Close()

	r, err := New(srv.URL).Poll(context.Background(), "gone")
	if err != nil || r.State != StateExpired {
		t.Fatalf("got %+v, %v", r, err)
	}
}

// The pairing response carries a Notion access token, so a non-TLS broker is
// refused rather than merely warned about.
func TestRejectsPlaintextBroker(t *testing.T) {
	c := New("http://broker.example")
	if _, err := c.Start(context.Background(), filepath.Join(t.TempDir(), "qr.png")); !errors.Is(err, ErrInsecureBroker) {
		t.Errorf("Start: got %v, want ErrInsecureBroker", err)
	}
	if _, err := c.Poll(context.Background(), "abc"); !errors.Is(err, ErrInsecureBroker) {
		t.Errorf("Poll: got %v, want ErrInsecureBroker", err)
	}
}

// The QR sends the user's phone wherever the broker says, so a verification
// URL pointing off-broker must not be turned into one.
func TestRejectsOffBrokerVerificationURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"device_code":"abc123","verification_url":"https://evil.example/go/abc123"}`)
	}))
	defer srv.Close()

	qrPath := filepath.Join(t.TempDir(), "qr.png")
	_, err := New(srv.URL).Start(context.Background(), qrPath)
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("Start: got %v, want an off-broker refusal", err)
	}
	if _, statErr := os.Stat(qrPath); statErr == nil {
		t.Error("a QR was written for a rejected verification URL")
	}
}

// The QR encodes a live pairing URL; it must not be world-readable.
func TestQRIsOwnerOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"device_code":"abc123","verification_url":%q}`, "http://127.0.0.1"+r.Host[strings.LastIndex(r.Host, ":"):]+"/go/user9")
	}))
	defer srv.Close()

	qrPath := filepath.Join(t.TempDir(), "qr.png")
	c := New(srv.URL)
	if _, err := c.Start(context.Background(), qrPath); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(qrPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("QR mode = %04o, want 0600", fi.Mode().Perm())
	}
}
