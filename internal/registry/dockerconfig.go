package registry

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
)

// Auth is one registry credential for WriteDockerConfig.
type Auth struct{ Host, Username, Token string }

// dockerConfigHost maps a user-entered registry host to the key the docker CLI looks credentials up
// under. Docker Hub is the special case — its canonical key is the legacy v1 URL.
func dockerConfigHost(h string) string {
	switch canonicalRegistry(h) {
	case "docker.io":
		return "https://index.docker.io/v1/"
	default:
		return canonicalRegistry(h)
	}
}

// canonicalRegistry mirrors config.canonicalRegistry (kept local to avoid an import cycle): it
// collapses Docker Hub's spellings and strips scheme/trailing slash for stable matching.
func canonicalRegistry(h string) string {
	h = trimScheme(h)
	switch h {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return "docker.io"
	}
	return h
}

func trimScheme(h string) string {
	for _, p := range []string{"https://", "http://"} {
		if len(h) >= len(p) && h[:len(p)] == p {
			h = h[len(p):]
		}
	}
	for len(h) > 0 && (h[len(h)-1] == '/') {
		h = h[:len(h)-1]
	}
	return h
}

// WriteDockerConfig writes a docker CLI config.json into dir containing the given registry auths, so
// that `docker compose up --pull` (which reads this file and sends X-Registry-Auth to the daemon) can
// pull from private registries. Point DOCKER_CONFIG at dir. Entries with a blank username are skipped;
// with no usable entries the file is written empty so stale credentials don't linger.
func WriteDockerConfig(dir string, auths []Auth) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	entries := map[string]map[string]string{}
	for _, a := range auths {
		if a.Username == "" {
			continue
		}
		entries[dockerConfigHost(a.Host)] = map[string]string{
			"auth": base64.StdEncoding.EncodeToString([]byte(a.Username + ":" + a.Token)),
		}
	}
	b, err := json.MarshalIndent(map[string]any{"auths": entries}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600)
}

// MergeDockerConfig adds/updates a single registry auth in the config.json at dir while preserving any
// existing auths, credHelpers, and other fields (unlike WriteDockerConfig, which writes a fresh file).
// Agents use it to blend a server-pushed credential with whatever `docker login` the host already has.
// A blank username is a no-op.
func MergeDockerConfig(dir string, a Auth) error {
	if a.Username == "" {
		return nil
	}
	path := filepath.Join(dir, "config.json")
	doc := map[string]json.RawMessage{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &doc) // best-effort; a corrupt/missing file just starts fresh
	}
	auths := map[string]map[string]string{}
	if raw, ok := doc["auths"]; ok {
		_ = json.Unmarshal(raw, &auths)
	}
	auths[dockerConfigHost(a.Host)] = map[string]string{
		"auth": base64.StdEncoding.EncodeToString([]byte(a.Username + ":" + a.Token)),
	}
	ab, err := json.Marshal(auths)
	if err != nil {
		return err
	}
	doc["auths"] = ab
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}
