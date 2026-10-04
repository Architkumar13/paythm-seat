package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerRouting(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("root"))
	})
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("live"))
	})

	testCases := []struct {
		url      string
		expected string
	}{
		{"/health/live", "live"},
		{"/api?path=health/live", "live"},
		{"/api?path=/health/live", "live"},
	}

	for _, tc := range testCases {
		req := httptest.NewRequest("GET", tc.url, nil)
		if p := req.URL.Query().Get("path"); p != "" {
			if !strings.HasPrefix(p, "/") {
				p = "/" + p
			}
			u := *req.URL
			u.Path = p
			u.RawPath = p
			q := u.Query()
			q.Del("path")
			u.RawQuery = q.Encode()
			req.URL = &u
			req.RequestURI = u.RequestURI()
		}

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Body.String() != tc.expected {
			t.Errorf("url %s: got %q, want %q (req.URL.Path: %q)", tc.url, rec.Body.String(), tc.expected, req.URL.Path)
		}
	}
}
