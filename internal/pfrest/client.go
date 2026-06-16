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
	base   string // e.g. https://192.168.7.x/api/v2
	apiKey string
	http   *http.Client
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
	// Timeout per request (default 30s).
	Timeout time.Duration
}

// NewClient builds a Client. It does not contact pfSense until the first API
// call.
func NewClient(c Config) *Client {
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: c.Insecure}, //nolint:gosec // self-signed mgmt cert
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
		base:   fmt.Sprintf("%s://%s/api/v2", scheme, host),
		apiKey: c.APIKey,
		http:   &http.Client{Timeout: c.Timeout, Transport: tr},
	}
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
		return nil, fmt.Errorf("pfrest %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var env envelope
	// The envelope is always JSON; tolerate a non-JSON body only to surface it
	// in the error.
	_ = json.Unmarshal(raw, &env)
	if resp.StatusCode/100 != 2 {
		return nil, &APIError{
			Method:  method,
			Path:    path,
			Status:  resp.StatusCode,
			Message: env.Message,
			Body:    string(raw),
		}
	}
	return &env, nil
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
