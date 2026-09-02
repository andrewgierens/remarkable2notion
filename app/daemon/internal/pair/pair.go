// Package pair drives the device side of the pairing flow: ask the broker
// for a device code, render the verification URL as a QR PNG for the e-ink
// display, and poll until the user finishes the Notion consent screen.
package pair

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

// ErrInsecureBroker is returned when the configured broker is not reachable
// over TLS. The pairing response carries a Notion access token in the clear,
// so plaintext HTTP is refused outright rather than merely warned about.
var ErrInsecureBroker = errors.New("pair: broker URL must be https://")

// State of an in-flight pairing.
type State string

const (
	StatePending State = "pending"
	StateOK      State = "ok"
	StateExpired State = "expired"
)

// Session is one pairing attempt.
type Session struct {
	DeviceCode      string `json:"device_code"`
	VerificationURL string `json:"verification_url"`
	QRPNGPath       string `json:"qr_png_path"`
}

// PollResult is the outcome of one poll.
type PollResult struct {
	State     State
	Token     string
	Workspace string
}

// Client talks to the pairing broker.
type Client struct {
	BrokerURL string
	HTTP      *http.Client
}

// New returns a client for the given broker base URL.
func New(brokerURL string) *Client {
	return &Client{
		BrokerURL: strings.TrimRight(brokerURL, "/"),
		HTTP:      &http.Client{Timeout: 30 * time.Second},
	}
}

// checkBroker rejects a broker that would carry the token in the clear.
// Loopback is allowed so the flow can be exercised against a local broker.
func (c *Client) checkBroker() error {
	u, err := url.Parse(c.BrokerURL)
	if err != nil {
		return fmt.Errorf("pair: bad broker URL: %w", err)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		if host := u.Hostname(); host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return nil
		}
	}
	return fmt.Errorf("%w (got %q)", ErrInsecureBroker, c.BrokerURL)
}

// Start mints a device code and writes the QR image to qrPath.
func (c *Client) Start(ctx context.Context, qrPath string) (*Session, error) {
	if err := c.checkBroker(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BrokerURL+"/pair", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pair: broker unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pair: broker returned HTTP %d", resp.StatusCode)
	}
	var s Session
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&s); err != nil {
		return nil, fmt.Errorf("pair: decode broker response: %w", err)
	}
	if s.DeviceCode == "" || s.VerificationURL == "" {
		return nil, fmt.Errorf("pair: broker response incomplete")
	}
	// The QR is what the user's phone is sent to. A broker that answered with
	// some other host — or with plaintext http — would silently redirect the
	// consent flow, so hold it to the same bar as the broker itself.
	if !strings.HasPrefix(s.VerificationURL, c.BrokerURL+"/") {
		return nil, fmt.Errorf("pair: broker returned a verification URL outside %s", c.BrokerURL)
	}

	// High error correction so the QR survives e-ink ghosting; 512px scales
	// cleanly on both device resolutions.
	png, err := qrcode.Encode(s.VerificationURL, qrcode.High, 512)
	if err != nil {
		return nil, fmt.Errorf("pair: render QR: %w", err)
	}
	// The QR encodes a live pairing URL, so write it owner-only rather than
	// letting qrcode.WriteFile pick 0644.
	if err := os.WriteFile(qrPath, png, 0o600); err != nil {
		return nil, fmt.Errorf("pair: write QR: %w", err)
	}
	s.QRPNGPath = qrPath
	return &s, nil
}

// Poll asks the broker whether the pairing has completed. The token is
// delivered exactly once; a completed code polled again reports expired.
func (c *Client) Poll(ctx context.Context, deviceCode string) (*PollResult, error) {
	if err := c.checkBroker(); err != nil {
		return nil, err
	}
	// The device code reaches us over the socket, so escape it rather than
	// letting a crafted value reshape the request path.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BrokerURL+"/pair/"+url.PathEscape(deviceCode), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pair: broker unreachable: %w", err)
	}
	defer resp.Body.Close()

	var body struct {
		State       string `json:"state"`
		AccessToken string `json:"access_token"`
		Workspace   string `json:"workspace"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("pair: decode poll response: %w", err)
	}
	switch State(body.State) {
	case StateOK:
		return &PollResult{State: StateOK, Token: body.AccessToken, Workspace: body.Workspace}, nil
	case StatePending:
		return &PollResult{State: StatePending}, nil
	default:
		return &PollResult{State: StateExpired}, nil
	}
}
