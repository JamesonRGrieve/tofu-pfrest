// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Package pfrest is a minimal client for the pfSense REST API v2 served by the
// pfSense-pkg-RESTAPI package (https://pfrest.org). Transport is plain HTTPS
// with a stateless per-request API key (the `X-API-Key` header) — there is no
// login/session step, unlike cookie- or token-session APIs.
//
// The API is generic over its surface (any `/api/v2/...` path); this client is
// the thin transport the generic `pfrest_object` resource is built on. The
// response is always the envelope {code,status,response_id,message,data,...};
// callers extract the object(s) from `data`.
package pfrest

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a stateless pfSense REST API v2 client. Every request carries the
// `X-API-Key` header; there is no session to establish or tear down, so one
// Client is freely shared across resources (the provider does) and is safe for
// concurrent use (it holds no mutable per-request state).
type Client struct {
	base    string // e.g. https://192.168.7.x/api/v2
	apiKey  string
	http    *http.Client
	retries int
}

// Config configures a Client.
type Config struct {
	// Host is the pfSense address (host or host:port), no scheme.
	Host string
	// APIKey is a pfSense REST API key (System > REST API > Keys, or POST
	// /api/v2/auth/key). Sent as the `X-API-Key` header on every request.
	APIKey string
	// Insecure skips TLS verification (pfSense ships a self-signed cert; true is
	// the norm on a lab / OOB management network).
	Insecure bool
	// Timeout per request (default 180s). pfSense REST reads on a
	// pfBlockerNG-heavy box over a high-latency tunnel (e.g. omg over WireGuard)
	// can take well over 30s for alias/nat/vlan/interface endpoints; a tight
	// timeout fails those reads during import/refresh. 180s is generous headroom
	// for adoption without hanging indefinitely.
	Timeout time.Duration
	// Retries is the number of ADDITIONAL attempts on a transient transport
	// failure (dial/connect timeout, reset, EOF) or a 5xx, with exponential
	// backoff. A flaky high-latency tunnel (omg over WireGuard) intermittently
	// drops SYNs, so a single read can fail where a retry succeeds. Default 4.
	Retries int
	// DialTimeout bounds the TCP connect per attempt so a dropped-SYN connect
	// fails fast (and is retried) instead of waiting out the OS SYN-retry budget
	// (~130s). Default 30s — generous for a high-latency tunnel, well below the
	// per-request Timeout.
	DialTimeout time.Duration
}

// NewClient builds a Client. It does not contact pfSense until the first API
// call.
func NewClient(c Config) *Client {
	if c.Timeout == 0 {
		c.Timeout = 180 * time.Second
	}
	if c.Retries == 0 {
		c.Retries = 4
	}
	if c.DialTimeout == 0 {
		c.DialTimeout = 30 * time.Second
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: c.Insecure}, //nolint:gosec // self-signed mgmt cert
		DialContext: (&net.Dialer{
			Timeout:   c.DialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:    4,
		IdleConnTimeout: 30 * time.Second,
	}
	// Scheme is taken from a `http://` / `https://` prefix on Host (default
	// https). pfSense REST API is normally HTTPS on the webConfigurator port, but
	// some deployments expose it over plain HTTP on a custom port (e.g.
	// http://host:8080) — honour that instead of forcing https.
	host := strings.TrimSuffix(c.Host, "/")
	scheme := "https"
	if strings.HasPrefix(host, "http://") {
		scheme = "http"
	}
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	return &Client{
		base:    fmt.Sprintf("%s://%s/api/v2", scheme, host),
		apiKey:  c.APIKey,
		http:    &http.Client{Timeout: c.Timeout, Transport: tr},
		retries: c.Retries,
	}
}

// backoff is the exponential delay before retry attempt N (0-based), capped at
// 16s: 1s, 2s, 4s, 8s, 16s, 16s…
func backoff(attempt int) time.Duration {
	d := (time.Duration(1) << uint(attempt)) * time.Second
	if d > 16*time.Second {
		d = 16 * time.Second
	}
	return d
}

// APIError is returned when pfSense responds with a non-2xx status. Message is
// the human-readable `message` from the response envelope when present, else
// the raw body.
type APIError struct {
	Method  string
	Path    string
	Status  int
	Message string
	Body    string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Body
	}
	return fmt.Sprintf("pfrest %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, msg)
}

// NotFound reports whether err is an APIError with a 404 status.
func NotFound(err error) bool {
	var ae *APIError
	if e, ok := err.(*APIError); ok {
		ae = e
	}
	return ae != nil && ae.Status == http.StatusNotFound
}

// envelope is the pfSense REST API v2 response wrapper. `data` carries the
// object (singular endpoint) or array of objects (plural endpoint); it is held
// raw so callers decide how to interpret it.
type envelope struct {
	Code       int             `json:"code"`
	Status     string          `json:"status"`
	ReturnID   int             `json:"return"`
	Message    string          `json:"message"`
	Data       json.RawMessage `json:"data"`
	ResponseID string          `json:"response_id"`
}

// do performs one authenticated request and returns the parsed envelope on a
// 2xx, or an APIError otherwise. path is relative to /api/v2 and must start
// with "/". query is optional (nil for none). body may be nil.
func (c *Client) do(method, path string, query url.Values, body []byte) (*envelope, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, u, rdr)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-API-Key", c.apiKey)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			// Transport-level failure (dial/connect timeout, reset, EOF) — transient
			// over a flaky high-latency tunnel; retry with backoff before giving up.
			lastErr = fmt.Errorf("pfrest %s %s: %w", method, path, err)
			if attempt < c.retries {
				time.Sleep(backoff(attempt))
				continue
			}
			return nil, lastErr
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		var env envelope
		// The envelope is always JSON; tolerate a non-JSON body only to surface it
		// in the error.
		_ = json.Unmarshal(raw, &env)
		if resp.StatusCode/100 != 2 {
			apiErr := &APIError{
				Method:  method,
				Path:    path,
				Status:  resp.StatusCode,
				Message: env.Message,
				Body:    string(raw),
			}
			// 5xx is transient (server busy / mid-filter-reload); 4xx is definitive.
			if resp.StatusCode/100 == 5 && attempt < c.retries {
				time.Sleep(backoff(attempt))
				continue
			}
			return nil, apiErr
		}
		return &env, nil
	}
}

// Get fetches a resource. query carries the id for singular collection items
// (e.g. id=3); nil for singletons and plural list endpoints. Returns the raw
// `data` field of the envelope.
func (c *Client) Get(path string, query url.Values) (json.RawMessage, error) {
	env, err := c.do(http.MethodGet, path, query, nil)
	if err != nil {
		return nil, err
	}
	return env.Data, nil
}

// Post creates an object in a collection with the given JSON body. Returns the
// raw `data` of the created object (which includes the server-assigned `id`).
func (c *Client) Post(path string, body []byte) (json.RawMessage, error) {
	env, err := c.do(http.MethodPost, path, nil, body)
	if err != nil {
		return nil, err
	}
	return env.Data, nil
}

// Patch updates an object (singular collection item or singleton) with the
// given JSON body. For a collection item the id must be carried inside body
// (the pfSense convention) and/or in query. Returns the raw `data`.
func (c *Client) Patch(path string, query url.Values, body []byte) (json.RawMessage, error) {
	env, err := c.do(http.MethodPatch, path, query, body)
	if err != nil {
		return nil, err
	}
	return env.Data, nil
}

// Delete removes a collection object addressed by query (id=N).
func (c *Client) Delete(path string, query url.Values) (json.RawMessage, error) {
	env, err := c.do(http.MethodDelete, path, query, nil)
	if err != nil {
		return nil, err
	}
	return env.Data, nil
}
