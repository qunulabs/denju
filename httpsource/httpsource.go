// Package httpsource implements [denju.Source] over plain HTTP.
//
// It is a convenience, not a requirement. denju's core has no network imports at
// all, and a program that fetches its updates over gRPC, from an object store,
// or off a shared filesystem simply implements [denju.Source] itself - the
// interface is one method.
//
// What this package deliberately does NOT do is establish trust. It performs a
// GET and streams the response body; denju then checks the digest the caller
// supplied. Nothing here validates a signature or decides that a host is
// trustworthy. Point it at a pre-signed URL from a channel you already trust,
// and carry the digest over that same channel.
package httpsource

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/qunulabs/denju"
)

// Option configures a Source.
type Option func(*source)

// WithClient sets the HTTP client. Defaults to http.DefaultClient.
//
// Supply your own to control TLS, proxies, or redirect policy - a redirect off
// the host you meant to talk to is the sort of thing worth refusing explicitly.
func WithClient(c *http.Client) Option {
	return func(s *source) {
		if c != nil {
			s.client = c
		}
	}
}

// WithMaxBytes refuses a response longer than n bytes. Zero, the default, means
// no limit.
//
// Worth setting. Without it a misconfigured or hostile endpoint can fill the
// filesystem the program is installed on, which takes down more than the update.
func WithMaxBytes(n int64) Option {
	return func(s *source) { s.maxBytes = n }
}

// WithHeader adds a header to every request.
func WithHeader(key, value string) Option {
	return func(s *source) {
		if s.header == nil {
			s.header = http.Header{}
		}
		s.header.Add(key, value)
	}
}

type source struct {
	build    func(context.Context, denju.Request) (*http.Request, error)
	client   *http.Client
	header   http.Header
	maxBytes int64
}

// New returns a Source that GETs a fixed URL.
func New(url string, opts ...Option) denju.Source {
	return NewFunc(func(ctx context.Context, _ denju.Request) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	}, opts...)
}

// NewFunc returns a Source that builds its request from the update request.
//
// Use it when the URL is not known until the update is announced, which is the
// normal case for a pre-signed link, or when the request needs per-update
// authentication.
func NewFunc(build func(context.Context, denju.Request) (*http.Request, error), opts ...Option) denju.Source {
	s := &source{build: build, client: http.DefaultClient}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Fetch implements [denju.Source].
func (s *source) Fetch(ctx context.Context, req denju.Request, w io.Writer) error {
	httpReq, err := s.build(ctx, req)
	if err != nil {
		return fmt.Errorf("build the update request: %w", err)
	}
	for k, vs := range s.header {
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the update server answered %s", resp.Status)
	}

	body := io.Reader(resp.Body)
	if s.maxBytes > 0 {
		// One byte past the limit, so a response exactly at the limit still
		// succeeds and anything longer is detectable rather than silently
		// truncated into a digest mismatch.
		body = io.LimitReader(resp.Body, s.maxBytes+1)
	}

	n, err := io.Copy(w, body)
	if err != nil {
		return err
	}
	if s.maxBytes > 0 && n > s.maxBytes {
		return fmt.Errorf("the update is larger than the %d byte limit", s.maxBytes)
	}
	return nil
}
