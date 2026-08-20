package selfupdate

import (
	"encoding/json"
	"strings"
	"testing"
)

// A realistic `docker inspect` fragment for the belay container on Galaxy.
const sample = `[{
  "Name": "/belay",
  "Image": "sha256:old",
  "Config": {
    "Env": ["PATH=/usr/bin", "DOCKER_HOST=tcp://belay-sockproxy:2375", "BELAY_PASSWORD=secret", "BELAY_DATA_DIR=/var/lib/belay"],
    "Cmd": ["server", "--addr", "0.0.0.0:8080"],
    "Image": "belay:latest",
    "Labels": {
      "com.docker.compose.project": "belay",
      "com.docker.compose.service": "belay",
      "com.docker.compose.project.config_files": "/srv/infra/belay/docker-compose.yml",
      "traefik.http.routers.belay.rule": "Host(belay.kalostech.es)",
      "traefik.http.routers.belay.middlewares": "authentik-belay@docker"
    }
  },
  "HostConfig": {
    "RestartPolicy": {"Name": "unless-stopped"},
    "PortBindings": {"8080/tcp": [{"HostIp": "127.0.0.1", "HostPort": "8480"}]}
  },
  "Mounts": [
    {"Type": "bind", "Source": "/srv", "Destination": "/srv", "RW": true},
    {"Type": "volume", "Name": "belay-data", "Source": "/var/lib/docker/volumes/belay-data/_data", "Destination": "/var/lib/belay", "RW": true}
  ],
  "NetworkSettings": {"Networks": {"belaynet": {"Aliases": []}}}
}]`

func TestRecreateScript(t *testing.T) {
	var docs []inspectDoc
	if err := json.Unmarshal([]byte(sample), &docs); err != nil {
		t.Fatal(err)
	}
	s := recreateScript(docs[0], "belay-previous", docs[0].Config.Image, true)

	must := []string{
		"docker run -d --name belay",
		"--restart unless-stopped",
		"-e BELAY_PASSWORD=secret",
		"-e DOCKER_HOST=tcp://belay-sockproxy:2375",
		"-v /srv:/srv",                 // bind, rw (no :ro)
		"-v belay-data:/var/lib/belay", // volume by NAME, not host path
		"-p 127.0.0.1:8480:8080/tcp",
		"--network belaynet",
		"belay:latest server --addr 0.0.0.0:8080", // image then cmd
	}
	for _, m := range must {
		if !strings.Contains(s, m) {
			t.Errorf("recreate script missing %q\n---\n%s", m, s)
		}
	}
	// a read-write bind must NOT be marked read-only
	if strings.Contains(s, "/srv:/srv:ro") {
		t.Error("rw bind was marked :ro")
	}
}

func TestRecreateScript_ReadOnlyMount(t *testing.T) {
	var d inspectDoc
	d.Name, d.Config.Image = "/x", "x:1"
	d.Mounts = append(d.Mounts, struct {
		Type, Name, Source, Destination string
		RW                              bool
	}{Type: "bind", Source: "/etc/x", Destination: "/etc/x", RW: false})
	if !strings.Contains(recreateScript(d, "x-previous", d.Config.Image, true), "-v /etc/x:/etc/x:ro") {
		t.Error("read-only bind not rendered with :ro")
	}
}

// A self-update that drops labels orphans the container from its Compose project and, behind a
// reverse proxy, deletes the routing that puts Belay on the internet. Updating yourself off the
// network is the single worst outcome for a tool whose whole job is safe updates.
func TestRecreateScript_PreservesLabels(t *testing.T) {
	s := script(t)
	for _, want := range []string{
		"--label com.docker.compose.project=belay",
		"--label com.docker.compose.service=belay",
		"--label com.docker.compose.project.config_files=/srv/infra/belay/docker-compose.yml",
		// parentheses force shell quoting — a Traefik rule must survive the round trip intact
		"--label 'traefik.http.routers.belay.rule=Host(belay.kalostech.es)'",
		"--label traefik.http.routers.belay.middlewares=authentik-belay@docker",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("recreate script drops %q\n---\n%s", want, s)
		}
	}
}
