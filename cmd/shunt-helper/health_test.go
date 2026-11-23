package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbeURLJudgesStatusCodes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) {})
	mux.HandleFunc("/fail", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })
	mux.HandleFunc("/to-ok", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ok", http.StatusFound) })
	mux.HandleFunc("/to-fail", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/fail", http.StatusFound) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cases := []struct {
		path    string
		follow  bool
		healthy bool
	}{
		{"/ok", false, true},
		{"/fail", false, false},
		// A bare redirect only proves the server listens; the historical default
		// accepts it.
		{"/to-fail", false, true},
		// follow asks for the page at the end of the chain.
		{"/to-ok", true, true},
		{"/to-fail", true, false},
	}
	for _, c := range cases {
		_, err := probeURL(srv.URL+c.path, c.follow)
		if got := err == nil; got != c.healthy {
			t.Errorf("%s follow=%v: healthy=%v, want %v (%v)", c.path, c.follow, got, c.healthy, err)
		}
	}

	if _, err := probeURL("http://127.0.0.1:1/", false); err == nil {
		t.Error("a port nothing listens on probed healthy")
	}
}
