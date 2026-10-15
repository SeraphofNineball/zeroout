// Package web serves the ZeroOut UI and its JSON API.
package web

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"mime"
	"net/http"

	"zeroout/internal/job"
)

//go:embed static
var staticFiles embed.FS

// Handler returns the UI and API. If password is non-empty every route except
// /api/health requires HTTP basic auth with that password (any username).
func Handler(m *job.Manager, password string) http.Handler {
	static, _ := fs.Sub(staticFiles, "static")
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(static))

	mux.HandleFunc("GET /api/volumes", func(w http.ResponseWriter, r *http.Request) {
		vols, err := m.Volumes()
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, vols)
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, m.Status())
	})
	mux.HandleFunc("POST /api/jobs", func(w http.ResponseWriter, r *http.Request) {
		var spec job.Spec
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&spec); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		j, err := m.Start(spec)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, j)
	})
	mux.HandleFunc("POST /api/jobs/resume", func(w http.ResponseWriter, r *http.Request) {
		j, err := m.Resume()
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, j)
	})
	mux.HandleFunc("POST /api/jobs/cancel", func(w http.ResponseWriter, r *http.Request) {
		if err := m.Cancel(); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /api/jobs/resumable", func(w http.ResponseWriter, r *http.Request) {
		m.DiscardResumable()
		w.WriteHeader(http.StatusNoContent)
	})

	var h http.Handler = requireJSON(mux)
	if password != "" {
		h = basicAuth(password, h)
	}
	root := http.NewServeMux()
	root.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	root.Handle("/", h)
	return root
}

// requireJSON rejects state-changing requests that aren't sent as JSON. A
// cross-site form can't set that content type without a CORS preflight, which
// we never approve, so this blocks CSRF from other pages on the LAN.
func requireJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
				writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Content-Type must be application/json"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func basicAuth(password string, next http.Handler) http.Handler {
	want := []byte(password)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pw, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pw), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="ZeroOut"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, job.ErrBusy):
		status = http.StatusConflict
	case errors.Is(err, job.ErrInvalid):
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
