package api

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrUnauthorized means the host credential was revoked or the host was
// delisted. The agent stops calling when it sees it.
var ErrUnauthorized = errors.New("host credential rejected (revoked or delisted)")

// HTTPError is a non-2xx answer.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("host api: HTTP %d: %s", e.Status, e.Body)
}

// Retryable reports whether the call may succeed if repeated.
func (e *HTTPError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// Client calls the host API.
type Client struct {
	BaseURL    string
	Credential string
	UserAgent  string
	HTTP       *http.Client
}

// New returns a client with sane timeouts. HTTPS is required unless the
// base URL is a loopback address (tests).
func New(baseURL, credential, version string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("api url: %w", err)
	}
	if u.Scheme != "https" && !isLoopback(u.Hostname()) {
		return nil, fmt.Errorf("api url must be https: %s", baseURL)
	}
	return &Client{
		BaseURL:    u.String(),
		Credential: credential,
		UserAgent:  "runcrate-host/" + version,
		HTTP:       &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func isLoopback(h string) bool {
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

func (c *Client) do(ctx context.Context, method, path string, auth bool, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	if auth {
		if c.Credential == "" {
			return ErrUnauthorized
		}
		req.Header.Set("Authorization", "Bearer "+c.Credential)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if auth && resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := string(raw)
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return &HTTPError{Status: resp.StatusCode, Body: msg}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("host api: decode %s: %w", path, err)
	}
	return nil
}

// Register exchanges a registration token for a host credential.
func (c *Client) Register(ctx context.Context, req RegisterRequest) (*RegisterResponse, error) {
	var out RegisterResponse
	if err := c.do(ctx, http.MethodPost, "/register", false, req, &out); err != nil {
		return nil, err
	}
	if out.HostID == "" || out.HostCredential == "" {
		return nil, errors.New("host api: register answered without host_id or host_credential")
	}
	return &out, nil
}

// Heartbeat reports state and returns pending commands.
func (c *Client) Heartbeat(ctx context.Context, req HeartbeatRequest) (*HeartbeatResponse, error) {
	var out HeartbeatResponse
	if err := c.do(ctx, http.MethodPost, "/heartbeat", true, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Result posts the outcome of a command.
func (c *Client) Result(ctx context.Context, commandID int64, res CommandResult) error {
	return c.do(ctx, http.MethodPost, "/commands/"+strconv.FormatInt(commandID, 10)+"/result", true, res, nil)
}

// Benchmark posts benchmark results.
func (c *Client) Benchmark(ctx context.Context, req BenchmarkRequest) (*BenchmarkResponse, error) {
	var out BenchmarkResponse
	if err := c.do(ctx, http.MethodPost, "/benchmark", true, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Image asks for the VM base image. An empty version means the current one.
func (c *Client) Image(ctx context.Context, version string) (*Image, error) {
	path := "/image"
	if version != "" {
		path += "?version=" + url.QueryEscape(version)
	}
	var out Image
	if err := c.do(ctx, http.MethodGet, path, true, nil, &out); err != nil {
		return nil, err
	}
	if out.URL == "" || out.SHA256 == "" || out.Version == "" {
		return nil, errors.New("host api: image answer is missing url, sha256 or version")
	}
	return &out, nil
}

// RequestID derives a stable UUID (v5 shape) from a name, so retries of the
// same logical request carry the same request_id.
func RequestID(name string) string {
	// RFC 4122 URL namespace.
	ns := []byte{0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	h := sha1.New()
	h.Write(ns)
	h.Write([]byte("runcrate-host:" + name))
	s := h.Sum(nil)[:16]
	s[6] = (s[6] & 0x0f) | 0x50
	s[8] = (s[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", s[0:4], s[4:6], s[6:8], s[8:10], s[10:16])
}
