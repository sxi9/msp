package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// ErrUnauthorized is matched (errors.Is) by any 401 answer: the device token
// was revoked or the enrollment token is invalid.
var ErrUnauthorized = errors.New("unauthorized")

// HTTPError is returned for every non-2xx answer. Code and Detail come from
// the server's {"error","detail"} body when it could be parsed.
type HTTPError struct {
	Status int
	Code   string
	Detail string
}

func (e *HTTPError) Error() string {
	s := fmt.Sprintf("server returned %d", e.Status)
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	return s
}

// Is makes errors.Is(err, ErrUnauthorized) true for 401 answers.
func (e *HTTPError) Is(target error) bool {
	return target == ErrUnauthorized && e.Status == http.StatusUnauthorized
}

// Client talks to /api/agent/v1 on one server with one device token.
type Client struct {
	base    string
	http    *http.Client
	token   string
	version string
}

// NewClient applies the URL policy (https always; http only to loopback or
// with insecure_http), builds the TLS configuration (min TLS 1.2, optional
// private CA) and a 40 s timeout that comfortably covers the 25 s long-poll.
func NewClient(c Config, version string) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(c.ServerURL))
	if err != nil {
		return nil, fmt.Errorf("server url: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("server url %q has no host", c.ServerURL)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) && !c.InsecureHTTP {
			return nil, errors.New("refusing plain http:// server URL; use https:// or --insecure-http")
		}
	default:
		return nil, fmt.Errorf("server url scheme must be https (got %q)", u.Scheme)
	}
	u.RawQuery = ""
	u.Fragment = ""
	base := strings.TrimRight(u.String(), "/")

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("ca file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca file %s: no PEM certificates found", c.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	return &Client{
		base:    base,
		http:    &http.Client{Transport: tr, Timeout: 40 * time.Second},
		token:   c.DeviceToken,
		version: version,
	}, nil
}

// isLoopbackHost reports whether a URL host is localhost, 127.0.0.0/8 or ::1.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// UserAgent is msp-agent/<version> (<GOOS>/<GOARCH>).
func (c *Client) UserAgent() string {
	return fmt.Sprintf("msp-agent/%s (%s/%s)", c.version, runtime.GOOS, runtime.GOARCH)
}

// Enroll exchanges an enrollment token for a device token.
func (c *Client) Enroll(ctx context.Context, req protocol.EnrollRequest) (protocol.EnrollResponse, error) {
	var resp protocol.EnrollResponse
	err := c.do(ctx, "/api/agent/v1/enroll", req, &resp)
	return resp, err
}

// Sync is the heartbeat + long-poll command channel.
func (c *Client) Sync(ctx context.Context, req protocol.SyncRequest) (protocol.SyncResponse, error) {
	var resp protocol.SyncResponse
	err := c.do(ctx, "/api/agent/v1/sync", req, &resp)
	if err == nil && resp.Commands == nil {
		resp.Commands = []protocol.Command{}
	}
	return resp, err
}

// PostResult reports a finished command.
func (c *Client) PostResult(ctx context.Context, id int64, res protocol.CommandResult) error {
	return c.do(ctx, fmt.Sprintf("/api/agent/v1/commands/%d/result", id), res, nil)
}

// do POSTs in as JSON and decodes a 2xx body into out (when out != nil and
// the body is not empty). Any non-2xx answer becomes an *HTTPError.
func (c *Client) do(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent())
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		he := &HTTPError{Status: resp.StatusCode}
		var er protocol.ErrorResponse
		if json.Unmarshal(data, &er) == nil {
			he.Code = er.Error
			he.Detail = er.Detail
		}
		return he
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode %s response: %w", path, err)
	}
	return nil
}
