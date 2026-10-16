// Package web serves the ZeroOut UI and its JSON API.
package web

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"zeroout/internal/blockdev"
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
		if !decode(w, r, &spec) {
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

	// ----- scan & recover -----
	mux.HandleFunc("GET /api/volumes/{name}/device", func(w http.ResponseWriter, r *http.Request) {
		d, err := m.Device(r.PathValue("name"))
		var nf *blockdev.NotFoundError
		switch {
		case errors.As(err, &nf):
			writeJSON(w, http.StatusOK, map[string]any{"error": nf.Msg, "hostDev": nf.HostDev})
		case err != nil:
			writeJSON(w, http.StatusOK, map[string]any{"error": err.Error()})
		default:
			writeJSON(w, http.StatusOK, map[string]any{"device": d})
		}
	})
	mux.HandleFunc("GET /api/scans", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, m.Scans())
	})
	mux.HandleFunc("POST /api/scans", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Volume string `json:"volume"`
		}
		if !decode(w, r, &req) {
			return
		}
		j, err := m.StartScan(req.Volume)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, j)
	})
	mux.HandleFunc("GET /api/scans/{id}", func(w http.ResponseWriter, r *http.Request) {
		s, err := m.Scan(r.PathValue("id"))
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s)
	})
	mux.HandleFunc("DELETE /api/scans/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := m.DeleteScan(r.PathValue("id")); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/scans/{id}/items", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		iq := job.ItemQuery{Status: q.Get("status"), Sort: q.Get("sort")}
		if e := q.Get("exts"); e != "" {
			iq.Exts = strings.Split(e, ",")
		}
		iq.MinSize, _ = strconv.ParseInt(q.Get("minSize"), 10, 64)
		iq.Offset, _ = strconv.Atoi(q.Get("offset"))
		iq.Limit, _ = strconv.Atoi(q.Get("limit"))
		page, err := m.Items(r.PathValue("id"), iq)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, page)
	})
	mux.HandleFunc("GET /api/scans/{id}/items/{n}/content", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(r.PathValue("n"))
		if err != nil {
			writeError(w, job.ErrNotFound)
			return
		}
		c, err := m.OpenItem(r.PathValue("id"), n)
		if err != nil {
			writeError(w, err)
			return
		}
		name := fmt.Sprintf("recovered-%012x.%s", c.View.Offset, c.View.Ext)
		disp := "attachment"
		if r.URL.Query().Get("inline") == "1" && inlineSafe(c.MIME) {
			disp = "inline"
		}
		h := w.Header()
		h.Set("Content-Type", c.MIME)
		h.Set("Content-Disposition", fmt.Sprintf("%s; filename=%q", disp, name))
		// Carved data is untrusted: never let the browser sniff or run it.
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self'; media-src 'self'")
		h.Set("Cache-Control", "no-store")
		http.ServeContent(w, r, name, time.Time{}, c)
	})
	mux.HandleFunc("GET /api/destinations", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, m.Destinations())
	})
	mux.HandleFunc("POST /api/recover", func(w http.ResponseWriter, r *http.Request) {
		var spec job.RecoverSpec
		if !decode(w, r, &spec) {
			return
		}
		j, err := m.StartRecover(spec)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, j)
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

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return false
	}
	return true
}

// inlineSafe reports whether a carved file may be shown in the browser.
// Text types (HTML, SVG, ...) are served as text/plain, so they qualify too.
func inlineSafe(mime string) bool {
	return strings.HasPrefix(mime, "image/") && mime != "image/svg+xml" ||
		strings.HasPrefix(mime, "video/") || strings.HasPrefix(mime, "audio/") ||
		strings.HasPrefix(mime, "text/plain")
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, job.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, job.ErrOverwritten):
		status = http.StatusGone
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
