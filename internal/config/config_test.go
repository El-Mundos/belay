package config

import "testing"

func TestRegistryCredDockerHubAliases(t *testing.T) {
	s := Settings{Registries: []Registry{
		{Host: "docker.io", Username: "bob", Token: "pat"},
		{Host: "ghcr.io", Username: "eve", Token: "ght"},
	}}
	// Docker Hub's API host is registry-1.docker.io — it must still match the "docker.io" entry.
	for _, host := range []string{"docker.io", "registry-1.docker.io", "index.docker.io"} {
		u, tok, ok := s.RegistryCred(host)
		if !ok || u != "bob" || tok != "pat" {
			t.Errorf("%s: got (%q,%q,%v), want bob/pat/true", host, u, tok, ok)
		}
	}
	if u, _, ok := s.RegistryCred("ghcr.io"); !ok || u != "eve" {
		t.Errorf("ghcr.io: got (%q,%v)", u, ok)
	}
	if _, _, ok := s.RegistryCred("quay.io"); ok {
		t.Error("quay.io should not match")
	}
	// A blank-username entry must never be treated as usable creds.
	blank := Settings{Registries: []Registry{{Host: "docker.io", Username: "", Token: "x"}}}
	if _, _, ok := blank.RegistryCred("docker.io"); ok {
		t.Error("blank username should not match")
	}
}
