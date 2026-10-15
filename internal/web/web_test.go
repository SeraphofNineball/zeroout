package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zeroout/internal/job"
)

func newServer(t *testing.T, password string) *httptest.Server {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "targets", "vol"), 0o755)
	m, err := job.NewManager(job.Options{
		TargetsDir: filepath.Join(root, "targets"), DataDir: filepath.Join(root, "data"),
		AllowUnmounted: true, MaxBytesPerPass: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(Handler(m, password))
	t.Cleanup(s.Close)
	return s
}

func do(t *testing.T, method, url, contentType, body string, setAuth func(*http.Request)) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if setAuth != nil {
		setAuth(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestAPI(t *testing.T) {
	s := newServer(t, "")
	for _, c := range []struct {
		method, path, ct, body string
		want                   int
	}{
		{"GET", "/", "", "", 200},
		{"GET", "/app.js", "", "", 200},
		{"GET", "/api/volumes", "", "", 200},
		{"GET", "/api/status", "", "", 200},
		{"POST", "/api/jobs", "text/plain", `{"volume":"vol","passes":["zero"],"mode":"full"}`, 415},
		{"POST", "/api/jobs", "application/json", `{"volume":"vol","passes":["nope"],"mode":"full"}`, 400},
		{"POST", "/api/jobs", "application/json", `{"volume":"missing","passes":["zero"],"mode":"full"}`, 400},
		{"POST", "/api/jobs/cancel", "application/json", `{}`, 400},
		{"POST", "/api/jobs", "application/json", `{"volume":"vol","passes":["zero"],"mode":"full"}`, 201},
	} {
		if resp := do(t, c.method, s.URL+c.path, c.ct, c.body, nil); resp.StatusCode != c.want {
			t.Errorf("%s %s (%s): status %d, want %d", c.method, c.path, c.ct, resp.StatusCode, c.want)
		}
	}
}

func TestBasicAuth(t *testing.T) {
	s := newServer(t, "secret")
	if resp := do(t, "GET", s.URL+"/api/status", "", "", nil); resp.StatusCode != 401 {
		t.Errorf("no auth: %d", resp.StatusCode)
	}
	if resp := do(t, "GET", s.URL+"/api/status", "", "", func(r *http.Request) { r.SetBasicAuth("x", "wrong") }); resp.StatusCode != 401 {
		t.Errorf("wrong password: %d", resp.StatusCode)
	}
	if resp := do(t, "GET", s.URL+"/api/status", "", "", func(r *http.Request) { r.SetBasicAuth("anyone", "secret") }); resp.StatusCode != 200 {
		t.Errorf("right password: %d", resp.StatusCode)
	}
	if resp := do(t, "GET", s.URL+"/api/health", "", "", nil); resp.StatusCode != 200 {
		t.Errorf("health should not need auth: %d", resp.StatusCode)
	}
}
