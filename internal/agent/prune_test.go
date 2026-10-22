package agent

import (
	"strings"
	"testing"
)

func TestPruneTagsSharedID(t *testing.T) {
	// prom/prometheus:latest and :v3.13.1 were one image on galaxy: both go, the image with them.
	untag, why := pruneTags([]string{"prom/prometheus:latest", "prom/prometheus:v3.13.1"}, RepoOf("prom/prometheus:v3.13.1"))
	if why != "" || len(untag) != 2 {
		t.Fatalf("pruneTags = %v, %q; want both tags removed", untag, why)
	}
}

func TestPruneTagsNormalisesRegistry(t *testing.T) {
	if _, why := pruneTags([]string{"docker.io/prom/prometheus:v3"}, RepoOf("prom/prometheus:v3.1")); why != "" {
		t.Errorf("docker.io/ prefix should be the same repository, got %q", why)
	}
}

func TestPruneTagsLeavesOthersAlone(t *testing.T) {
	cases := map[string][]string{
		"foreign repo":   {"prom/prometheus:v3.13.1", "myregistry.local/prom-backup:keep"},
		"self-update":    {"belay:previous"},
		"snapshot image": {"alpine:3.20"},
	}
	for name, tags := range cases {
		if untag, why := pruneTags(tags, RepoOf("prom/prometheus:v3.13.1")); why == "" {
			t.Errorf("%s: pruneTags(%v) would remove %v; must keep the image", name, tags, untag)
		}
	}
}

func TestPruneTagsDangling(t *testing.T) {
	// a rebase leaves the old build with no tags at all: nothing to untag, removed by ID
	if untag, why := pruneTags(nil, "registry-1.docker.io/library/nginx"); why != "" || len(untag) != 0 {
		t.Errorf("pruneTags(nil) = %v, %q", untag, why)
	}
}

func TestRepoOfDigestRef(t *testing.T) {
	if got, want := RepoOf("ghcr.io/goauthentik/server@sha256:abc"), "ghcr.io/goauthentik/server"; got != want {
		t.Errorf("RepoOf(digest) = %q, want %q", got, want)
	}
}

func TestParseDFAvailTakesSmallest(t *testing.T) {
	out := "/dev/mapper/vg-var 48000000 29000000 18874368 61% /h/containerd\n" +
		"/dev/sda2 100000000 10000000 90000000 10% /h/docker\n"
	got, err := parseDFAvail(out)
	if err != nil || got != 18874368*1024 {
		t.Errorf("parseDFAvail = %d, %v; want the smaller filesystem", got, err)
	}
	if _, err := parseDFAvail("df: /h/containerd: No such file or directory\n"); err == nil {
		t.Error("garbage df output should be an error, not 0 free bytes")
	}
}

func TestPullNeedCoversMeasuredAuthentik(t *testing.T) {
	// measured on galaxy: 378 MB compressed occupied 2.32 GB once pulled
	if need := PullNeed(378e6); need < 2.32e9 {
		t.Errorf("PullNeed(378 MB) = %s, below the measured 2.32 GB", HumanBytes(need))
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{512: "512 B", 1940000000: "1.9 GB", 378000000: "378.0 MB"} {
		if got := HumanBytes(n); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", n, got, want)
		}
	}
	if !strings.HasSuffix(HumanBytes(1<<40), "TB") {
		t.Error("HumanBytes(1 TiB) should be in TB")
	}
}
