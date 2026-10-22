package journal

import "testing"

func TestJournalSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	e := Entry{File: "/srv/infra/authentik/docker-compose.yml", Service: "server",
		From: "ghcr.io/goauthentik/server:2026.8.4", To: "ghcr.io/goauthentik/server:2026.8.5"}
	j := Open(dir)
	if err := j.Begin(e); err != nil {
		t.Fatal(err)
	}
	_ = j.SetSnapshot(e.Key(), "server-1")

	// a fresh process reads what the killed one left
	p := Open(dir).Pending()
	if len(p) != 1 || p[0].Snapshot != "server-1" || p[0].Started.IsZero() {
		t.Fatalf("Pending after reopen = %+v", p)
	}
	_ = Open(dir).End(e.Key())
	if p := Open(dir).Pending(); len(p) != 0 {
		t.Fatalf("End did not clear the entry: %+v", p)
	}
}

func TestJournalDisabledWithoutDir(t *testing.T) {
	j := Open("")
	if err := j.Begin(Entry{Service: "x"}); err != nil || len(j.Pending()) != 0 {
		t.Fatal("an empty data dir must make the journal a no-op")
	}
}
