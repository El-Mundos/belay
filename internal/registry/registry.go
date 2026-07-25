// Package registry queries container registries (Docker Hub, ghcr, quay, gcr, …) for the tags of
// an image and reports which stable semver tags are newer than the one you're pinned to.
//
// It speaks the Docker Registry v2 API with the standard bearer-token dance (anonymous pull), so a
// single implementation covers the common registries.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Ref is a parsed image reference.
type Ref struct {
	Registry   string // API host, e.g. "registry-1.docker.io", "ghcr.io"
	Repository string // e.g. "library/nginx", "grafana/grafana-oss"
	Tag        string // e.g. "1.27.1"
}

// ParseRef splits an image string into registry/repository/tag, applying Docker Hub defaults.
func ParseRef(image string) Ref {
	r := Ref{Registry: "registry-1.docker.io"}
	name := image
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		r.Tag, name = name[i+1:], name[:i]
	}
	if i := strings.Index(name, "/"); i >= 0 {
		if first := name[:i]; strings.ContainsAny(first, ".:") || first == "localhost" {
			r.Registry, name = first, name[i+1:]
			if r.Registry == "docker.io" {
				r.Registry = "registry-1.docker.io"
			}
		}
	}
	if r.Registry == "registry-1.docker.io" && !strings.Contains(name, "/") {
		name = "library/" + name // official image
	}
	r.Repository = name
	return r
}

// Client queries registries. Scheme is overridable for tests.
type Client struct {
	HTTP   *http.Client
	Scheme string
}

func New() *Client {
	return &Client{HTTP: &http.Client{Timeout: 15 * time.Second}, Scheme: "https"}
}

// Tags returns all tags of the repository (following pagination + anonymous token auth).
func (c *Client) Tags(ctx context.Context, ref Ref) ([]string, error) {
	next := fmt.Sprintf("%s://%s/v2/%s/tags/list?n=200", c.Scheme, ref.Registry, ref.Repository)
	var (
		token string
		all   []string
	)
	for next != "" {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && token == "" {
			ch := parseChallenge(resp.Header.Get("WWW-Authenticate"))
			resp.Body.Close()
			if token, err = c.token(ctx, ch); err != nil {
				return nil, err
			}
			continue // retry same URL with the token
		}
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			return nil, fmt.Errorf("%s tags/list: %s: %s", ref.Repository, resp.Status, strings.TrimSpace(string(b)))
		}
		var body struct {
			Tags []string `json:"tags"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		link := resp.Header.Get("Link")
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		all = append(all, body.Tags...)
		next = c.nextLink(ref, link)
	}
	return all, nil
}

// Newer returns the stable tags strictly newer than ref.Tag, of the same version shape, ascending.
// comparable is false when ref.Tag itself isn't a clean semver-ish tag (e.g. "16-alpine", "latest").
func (c *Client) Newer(ctx context.Context, ref Ref) (newer []string, comparable bool, err error) {
	cur, ok := parseVer(ref.Tag)
	if !ok {
		return nil, false, nil
	}
	tags, err := c.Tags(ctx, ref)
	if err != nil {
		return nil, true, err
	}
	type tv struct {
		tag string
		v   []int
	}
	var list []tv
	for _, t := range tags {
		v, ok := parseVer(t)
		if !ok || len(v) != len(cur) { // same shape only (X.Y.Z vs X.Y.Z)
			continue
		}
		if cmpVer(v, cur) > 0 {
			list = append(list, tv{t, v})
		}
	}
	sort.Slice(list, func(i, j int) bool { return cmpVer(list[i].v, list[j].v) < 0 })
	for _, x := range list {
		newer = append(newer, x.tag)
	}
	return newer, true, nil
}

func (c *Client) token(ctx context.Context, ch map[string]string) (string, error) {
	realm := ch["realm"]
	if realm == "" {
		return "", fmt.Errorf("registry auth challenge had no realm")
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if s := ch["service"]; s != "" {
		q.Set("service", s)
	}
	if s := ch["scope"]; s != "" {
		q.Set("scope", s)
	}
	u.RawQuery = q.Encode()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint: %s", resp.Status)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return "", err
	}
	if t.Token != "" {
		return t.Token, nil
	}
	return t.AccessToken, nil
}

func (c *Client) nextLink(ref Ref, link string) string {
	if link == "" || !strings.Contains(link, `rel="next"`) {
		return ""
	}
	s, e := strings.Index(link, "<"), strings.Index(link, ">")
	if s < 0 || e < 0 || e < s {
		return ""
	}
	p := link[s+1 : e]
	if strings.HasPrefix(p, "http") {
		return p
	}
	return fmt.Sprintf("%s://%s%s", c.Scheme, ref.Registry, p)
}

// SourceRepo returns the image's source repository from its OCI `org.opencontainers.image.source`
// label (empty string if the image doesn't set one). Best-effort: it fetches the manifest, follows
// a multi-arch index to a concrete manifest, then reads the config blob's labels.
func (c *Client) SourceRepo(ctx context.Context, ref Ref) (string, error) {
	const accept = "application/vnd.oci.image.index.v1+json," +
		"application/vnd.docker.distribution.manifest.list.v2+json," +
		"application/vnd.oci.image.manifest.v1+json," +
		"application/vnd.docker.distribution.manifest.v2+json"
	token := ""
	man, ct, err := c.registryGet(ctx, ref, "/manifests/"+ref.Tag, accept, &token)
	if err != nil {
		return "", err
	}
	if strings.Contains(ct, "index") || strings.Contains(ct, "list") {
		var idx struct {
			Manifests []struct {
				Digest   string                        `json:"digest"`
				Platform struct{ Architecture string } `json:"platform"`
			} `json:"manifests"`
		}
		json.Unmarshal(man, &idx)
		dig := ""
		for _, m := range idx.Manifests {
			if m.Platform.Architecture == "amd64" {
				dig = m.Digest
				break
			}
		}
		if dig == "" && len(idx.Manifests) > 0 {
			dig = idx.Manifests[0].Digest
		}
		if dig == "" {
			return "", nil
		}
		if man, _, err = c.registryGet(ctx, ref, "/manifests/"+dig, accept, &token); err != nil {
			return "", err
		}
	}
	var m struct {
		Config struct{ Digest string } `json:"config"`
	}
	json.Unmarshal(man, &m)
	if m.Config.Digest == "" {
		return "", nil
	}
	blob, _, err := c.registryGet(ctx, ref, "/blobs/"+m.Config.Digest, "*/*", &token)
	if err != nil {
		return "", err
	}
	var cfg struct {
		Config struct{ Labels map[string]string } `json:"config"`
	}
	json.Unmarshal(blob, &cfg)
	return cfg.Config.Labels["org.opencontainers.image.source"], nil
}

// registryGet does an authenticated GET against /v2/<repo><path>, handling the bearer-token dance.
func (c *Client) registryGet(ctx context.Context, ref Ref, path, accept string, token *string) ([]byte, string, error) {
	url := fmt.Sprintf("%s://%s/v2/%s%s", c.Scheme, ref.Registry, ref.Repository, path)
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if *token != "" {
			req.Header.Set("Authorization", "Bearer "+*token)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, "", err
		}
		if resp.StatusCode == http.StatusUnauthorized && *token == "" {
			ch := parseChallenge(resp.Header.Get("WWW-Authenticate"))
			resp.Body.Close()
			if *token, err = c.token(ctx, ch); err != nil {
				return nil, "", err
			}
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
			return nil, "", fmt.Errorf("%s%s: %s: %s", ref.Repository, path, resp.Status, strings.TrimSpace(string(b)))
		}
		body, err := io.ReadAll(resp.Body)
		return body, resp.Header.Get("Content-Type"), err
	}
}

// ChangelogURL turns a source repo + tag into a best-effort release-notes link.
func ChangelogURL(sourceRepo, tag string) string {
	s := strings.TrimSuffix(strings.TrimSpace(sourceRepo), ".git")
	switch {
	case s == "":
		return ""
	case strings.Contains(s, "github.com"):
		return s + "/releases/tag/" + tag
	case strings.Contains(s, "gitlab"):
		return s + "/-/releases/" + tag
	default:
		return s
	}
}

var challengeRe = regexp.MustCompile(`(\w+)="([^"]*)"`)

func parseChallenge(h string) map[string]string {
	m := map[string]string{}
	for _, kv := range challengeRe.FindAllStringSubmatch(strings.TrimPrefix(h, "Bearer "), -1) {
		m[kv[1]] = kv[2]
	}
	return m
}

var verRe = regexp.MustCompile(`^v?(\d+(?:\.\d+)*)$`)

// parseVer parses a clean version tag (e.g. "1.27.1", "v1.12.1", "2026.5.5", "v0.131") into its
// numeric components. Variant/pre-release tags ("16-alpine", "v3-rc.1", "latest") are rejected.
func parseVer(tag string) ([]int, bool) {
	m := verRe.FindStringSubmatch(tag)
	if m == nil {
		return nil, false
	}
	parts := strings.Split(m[1], ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out[i] = n
	}
	return out, true
}

// cmpVer compares two numeric version tuples, zero-padding the shorter.
func cmpVer(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}
