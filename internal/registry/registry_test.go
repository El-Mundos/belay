package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestParseRef(t *testing.T) {
	cases := map[string]Ref{
		"nginx:1.27.1":                   {"registry-1.docker.io", "library/nginx", "1.27.1"},
		"grafana/grafana-oss:13.0.2":     {"registry-1.docker.io", "grafana/grafana-oss", "13.0.2"},
		"docker.io/prom/prometheus:v3.0": {"registry-1.docker.io", "prom/prometheus", "v3.0"},
		"ghcr.io/prymitive/karma:v0.131": {"ghcr.io", "prymitive/karma", "v0.131"},
		"gcr.io/cadvisor/cadvisor:v0.55": {"gcr.io", "cadvisor/cadvisor", "v0.55"},
		"localhost:5000/app:1.0":         {"localhost:5000", "app", "1.0"},
	}
	for in, want := range cases {
		if got := ParseRef(in); got != want {
			t.Errorf("ParseRef(%q) = %+v, want %+v", in, got, want)
		}
	}
}

func TestVersionCompareAndFilter(t *testing.T) {
	if _, ok := parseVer("16-alpine"); ok {
		t.Error("16-alpine should not parse as clean version")
	}
	if _, ok := parseVer("v3.1.0-rc.1"); ok {
		t.Error("pre-release should not parse")
	}
	v, ok := parseVer("v1.12.1")
	if !ok || !reflect.DeepEqual(v, []int{1, 12, 1}) {
		t.Errorf("parseVer(v1.12.1) = %v, %v", v, ok)
	}
	if cmpVer([]int{1, 27, 1}, []int{1, 28, 0}) >= 0 {
		t.Error("1.27.1 should be < 1.28.0")
	}
	if cmpVer([]int{2026, 5, 5}, []int{2026, 5, 4}) <= 0 {
		t.Error("calver compare failed")
	}
}

// mockRegistry serves the v2 tags API with a token challenge, like a real registry.
func mockRegistry(t *testing.T, repo string, tags []string) (*Client, Ref) {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"token": "secret"})
	})
	mux.HandleFunc("/v2/"+repo+"/tags/list", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.Header().Set("WWW-Authenticate",
				`Bearer realm="`+srv.URL+`/token",service="test",scope="repository:`+repo+`:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": tags})
	})

	host := strings.TrimPrefix(srv.URL, "http://")
	return &Client{HTTP: srv.Client(), Scheme: "http"}, Ref{Registry: host, Repository: repo}
}

func TestTags_TokenAuth(t *testing.T) {
	c, ref := mockRegistry(t, "library/nginx", []string{"1.27.1", "1.27.2", "latest"})
	got, err := c.Tags(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("tags = %v", got)
	}
}

func TestNewer(t *testing.T) {
	tags := []string{"1.26.0", "1.27.0", "1.27.1", "1.27.2", "1.28.0", "2", "latest", "1.27.1-alpine"}
	c, ref := mockRegistry(t, "library/nginx", tags)
	ref.Tag = "1.27.1"
	newer, ok, err := c.Newer(context.Background(), ref)
	if err != nil || !ok {
		t.Fatalf("Newer err=%v comparable=%v", err, ok)
	}
	// same-shape, strictly newer, ascending — "2" and "-alpine" excluded, older excluded
	want := []string{"1.27.2", "1.28.0"}
	if !reflect.DeepEqual(newer, want) {
		t.Errorf("Newer = %v, want %v", newer, want)
	}

	ref.Tag = "16-alpine" // non-semver current -> not comparable
	if _, comparable, _ := c.Newer(context.Background(), ref); comparable {
		t.Error("16-alpine should be reported not-comparable")
	}
}
