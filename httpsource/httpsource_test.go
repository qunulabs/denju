package httpsource_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qunulabs/denju"
	"github.com/qunulabs/denju/httpsource"
)

func fetch(t *testing.T, src denju.Source, req denju.Request) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := src.Fetch(context.Background(), req, &buf)
	return buf.String(), err
}

func TestFetch_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("new binary bytes"))
	}))
	defer srv.Close()

	got, err := fetch(t, httpsource.New(srv.URL), denju.Request{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got != "new binary bytes" {
		t.Fatalf("body = %q, want the served bytes", got)
	}
}

func TestFetch_NonOKStatusIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := fetch(t, httpsource.New(srv.URL), denju.Request{})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("error = %v, want one naming the status", err)
	}
}

// A pre-signed URL is not known until the update is announced, so the common
// case is building the request from the update request.
func TestNewFunc_BuildsPerRequest(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte("bytes"))
	}))
	defer srv.Close()

	src := httpsource.NewFunc(func(ctx context.Context, req denju.Request) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v/"+req.TargetVersion, nil)
	})

	if _, err := fetch(t, src, denju.Request{TargetVersion: "1.5.0"}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if gotPath != "/v/1.5.0" {
		t.Fatalf("path = %q, want the version-derived path", gotPath)
	}
}

func TestWithHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("bytes"))
	}))
	defer srv.Close()

	src := httpsource.New(srv.URL, httpsource.WithHeader("Authorization", "Bearer token"))
	if _, err := fetch(t, src, denju.Request{}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got != "Bearer token" {
		t.Fatalf("Authorization = %q, want the configured header", got)
	}
}

// Without a cap, a misconfigured endpoint can fill the filesystem the program is
// installed on - which takes down considerably more than the update.
func TestWithMaxBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 100))
	}))
	defer srv.Close()

	t.Run("under the limit succeeds", func(t *testing.T) {
		got, err := fetch(t, httpsource.New(srv.URL, httpsource.WithMaxBytes(100)), denju.Request{})
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if len(got) != 100 {
			t.Fatalf("len(body) = %d, want 100", len(got))
		}
	})

	t.Run("over the limit fails", func(t *testing.T) {
		_, err := fetch(t, httpsource.New(srv.URL, httpsource.WithMaxBytes(99)), denju.Request{})
		if err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Fatalf("error = %v, want a size-limit error", err)
		}
	})

	t.Run("zero means unlimited", func(t *testing.T) {
		got, err := fetch(t, httpsource.New(srv.URL, httpsource.WithMaxBytes(0)), denju.Request{})
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if len(got) != 100 {
			t.Fatalf("len(body) = %d, want 100", len(got))
		}
	})
}

func TestFetch_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("bytes"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var buf bytes.Buffer
	if err := httpsource.New(srv.URL).Fetch(ctx, denju.Request{}, &buf); err == nil {
		t.Fatal("expected a cancelled request to fail")
	}
}

func TestWithClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("bytes"))
	}))
	defer srv.Close()

	used := false
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		used = true
		return http.DefaultTransport.RoundTrip(r)
	})}

	if _, err := fetch(t, httpsource.New(srv.URL, httpsource.WithClient(client)), denju.Request{}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !used {
		t.Fatal("the configured client must be used")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
