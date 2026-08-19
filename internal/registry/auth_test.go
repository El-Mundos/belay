package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthForBasic(t *testing.T) {
	c := New()
	c.SetCreds(func(host string) (string, string, bool) {
		if host == "registry.example.com:5000" {
			return "alice", "s3cret", true
		}
		return "", "", false
	})
	ref := Ref{Registry: "registry.example.com:5000", Repository: "app"}

	got, err := c.authFor(context.Background(), ref, `Basic realm="Registry"`)
	if err != nil {
		t.Fatalf("authFor: %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}

	// A Basic challenge with no matching creds must be a clear error, not a silent anonymous retry.
	if _, err := c.authFor(context.Background(), Ref{Registry: "other.io"}, "Basic realm=x"); err == nil {
		t.Fatal("expected error for missing credentials")
	}
}

func TestWriteDockerConfig(t *testing.T) {
	dir := t.TempDir()
	err := WriteDockerConfig(dir, []Auth{
		{Host: "docker.io", Username: "bob", Token: "pat"},
		{Host: "ghcr.io", Username: "eve", Token: "ght"},
		{Host: "skip.me", Username: "", Token: "x"}, // blank user -> skipped
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc struct {
		Auths map[string]struct{ Auth string } `json:"auths"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Docker Hub must land under the legacy v1 key.
	hub, ok := doc.Auths["https://index.docker.io/v1/"]
	if !ok {
		t.Fatalf("no docker hub key in %s", b)
	}
	if hub.Auth != base64.StdEncoding.EncodeToString([]byte("bob:pat")) {
		t.Fatalf("bad hub auth: %s", hub.Auth)
	}
	if _, ok := doc.Auths["ghcr.io"]; !ok {
		t.Fatal("no ghcr.io key")
	}
	if _, ok := doc.Auths["skip.me"]; ok {
		t.Fatal("blank-username row should have been skipped")
	}
	if strings.Contains(string(b), "\"x\"") {
		t.Fatal("skipped token leaked into file")
	}
}

func TestMergeDockerConfigPreservesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// Pre-existing host config: a manual login + a credHelper + an unrelated field.
	seed := `{
      "auths": {"ghcr.io": {"auth": "ZXhpc3Rpbmc="}},
      "credHelpers": {"gcr.io": "gcloud"},
      "psFormat": "table"
    }`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MergeDockerConfig(dir, Auth{Host: "docker.io", Username: "bob", Token: "pat"}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	b, _ := os.ReadFile(path)
	var doc struct {
		Auths       map[string]struct{ Auth string } `json:"auths"`
		CredHelpers map[string]string                `json:"credHelpers"`
		PsFormat    string                           `json:"psFormat"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Auths["ghcr.io"].Auth != "ZXhpc3Rpbmc=" {
		t.Error("existing ghcr.io login was clobbered")
	}
	if doc.Auths["https://index.docker.io/v1/"].Auth != base64.StdEncoding.EncodeToString([]byte("bob:pat")) {
		t.Error("pushed docker hub cred not merged")
	}
	if doc.CredHelpers["gcr.io"] != "gcloud" || doc.PsFormat != "table" {
		t.Error("non-auth fields were lost on merge")
	}
}
