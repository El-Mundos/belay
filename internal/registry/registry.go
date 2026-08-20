// Package registry queries container registries (Docker Hub, ghcr, quay, gcr, …) for the tags of
// an image and reports which stable semver tags are newer than the one you're pinned to.
//
// It speaks the Docker Registry v2 API with the standard bearer-token dance (anonymous pull), so a
// single implementation covers the common registries.
package registry

import (
	"context"
	"encoding/base64"
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
	// creds returns credentials for a registry host (empty ok => anonymous). Set via SetCreds so it
	// reads live settings; a nil creds means every request is anonymous (the default).
	creds func(host string) (user, pass string, ok bool)
}

func New() *Client {
	return &Client{HTTP: &http.Client{Timeout: 15 * time.Second}, Scheme: "https"}
}

// SetCreds wires a live credentials source (typically settings) for private registries.
func (c *Client) SetCreds(fn func(host string) (user, pass string, ok bool)) { c.creds = fn }

func (c *Client) cred(host string) (string, string, bool) {
	if c.creds == nil {
		return "", "", false
	}
	return c.creds(host)
}

// authFor turns a 401's WWW-Authenticate challenge into the Authorization header value to retry with.
// It handles the Bearer token dance (Docker Hub, ghcr, quay, Harbor) and plain Basic auth (a
// registry:2 behind htpasswd), pulling credentials from SetCreds when the registry needs them.
func (c *Client) authFor(ctx context.Context, ref Ref, wwwAuth string) (string, error) {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(wwwAuth)), "basic") {
		user, pass, ok := c.cred(ref.Registry)
		if !ok {
			return "", fmt.Errorf("registry %s requires credentials", ref.Registry)
		}
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass)), nil
	}
	tok, err := c.token(ctx, ref, parseChallenge(wwwAuth))
	if err != nil {
		return "", err
	}
	return "Bearer " + tok, nil
}

// Tags returns all tags of the repository (following pagination + anonymous token auth).
func (c *Client) Tags(ctx context.Context, ref Ref) ([]string, error) {
	next := fmt.Sprintf("%s://%s/v2/%s/tags/list?n=200", c.Scheme, ref.Registry, ref.Repository)
	var (
		auth string
		all  []string
	)
	for next != "" {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && auth == "" {
			ww := resp.Header.Get("WWW-Authenticate")
			resp.Body.Close()
			if auth, err = c.authFor(ctx, ref, ww); err != nil {
				return nil, err
			}
			continue // retry same URL with the credentials
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
//
// Shape is deliberately strict: a service pinned to "15" is only ever offered "16", never "15.4.0".
// Rewriting it to "15.4.0" would silently convert a rolling tag the user chose into a frozen one and
// take away the auto-tracking that was the point of pinning "15". Movement *within* a rolling tag is
// not a version change at all — it is the same tag pointing at a new build — and is detected by
// comparing digests instead (see Digest), which also covers tags no version scheme can order at all,
// like "latest" or "stable".
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
		if !ok || len(v) != len(cur) { // same shape only (X.Y.Z vs X.Y.Z) — see the doc comment
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

// Digest returns the registry's current manifest digest for ref's exact tag — what a `docker pull`
// of that tag would fetch right now. Comparing it against the digest actually running locally is how
// Belay tracks tags that carry no version information of their own ("latest", "stable") and tags
// that are re-pointed in place ("15" moving from 15.0.0 to 15.4.0). No pull is involved.
func (c *Client) Digest(ctx context.Context, ref Ref) (string, error) {
	const accept = "application/vnd.oci.image.index.v1+json," +
		"application/vnd.docker.distribution.manifest.list.v2+json," +
		"application/vnd.oci.image.manifest.v1+json," +
		"application/vnd.docker.distribution.manifest.v2+json"
	auth := ""
	url := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", c.Scheme, ref.Registry, ref.Repository, ref.Tag)
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
		req.Header.Set("Accept", accept)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return "", err
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized && auth == "" {
			if auth, err = c.authFor(ctx, ref, resp.Header.Get("WWW-Authenticate")); err != nil {
				return "", err
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("%s manifest %s: %s", ref.Repository, ref.Tag, resp.Status)
		}
		d := resp.Header.Get("Docker-Content-Digest")
		if d == "" {
			return "", fmt.Errorf("%s manifest %s: registry returned no digest", ref.Repository, ref.Tag)
		}
		return d, nil
	}
}

func (c *Client) token(ctx context.Context, ref Ref, ch map[string]string) (string, error) {
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
	if user, pass, ok := c.cred(ref.Registry); ok { // authenticated token for a private repo
		req.SetBasicAuth(user, pass)
	}
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

// registryGet does an authenticated GET against /v2/<repo><path>, handling the auth dance. auth holds
// the full Authorization header value across the caller's calls so the challenge is answered once.
func (c *Client) registryGet(ctx context.Context, ref Ref, path, accept string, auth *string) ([]byte, string, error) {
	url := fmt.Sprintf("%s://%s/v2/%s%s", c.Scheme, ref.Registry, ref.Repository, path)
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if *auth != "" {
			req.Header.Set("Authorization", *auth)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, "", err
		}
		if resp.StatusCode == http.StatusUnauthorized && *auth == "" {
			ww := resp.Header.Get("WWW-Authenticate")
			resp.Body.Close()
			if *auth, err = c.authFor(ctx, ref, ww); err != nil {
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
