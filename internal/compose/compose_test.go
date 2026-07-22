package compose

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `# my stack
services:
  web:
    image: nginx:1.27.1   # pinned
    ports:
      - "80:80"
  db:
    image: "postgres:16-alpine"
    environment:
      POSTGRES_PASSWORD: secret
`

func write(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFindImage(t *testing.T) {
	p := write(t, sample)
	if got, _ := FindImage(p, "web"); got != "nginx:1.27.1" {
		t.Errorf("web image = %q", got)
	}
	if got, _ := FindImage(p, "db"); got != "postgres:16-alpine" {
		t.Errorf("db image = %q", got)
	}
	if _, err := FindImage(p, "nope"); err == nil {
		t.Error("expected error for missing service")
	}
}

func TestSetImage_PreservesFormatting(t *testing.T) {
	p := write(t, sample)
	if err := SetImage(p, "web", "nginx:1.27.2"); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(p)
	s := string(out)
	if !strings.Contains(s, "image: nginx:1.27.2   # pinned") {
		t.Errorf("tag not rewritten in place / comment lost:\n%s", s)
	}
	// the quoted db image and the header comment must be untouched
	if !strings.Contains(s, `image: "postgres:16-alpine"`) || !strings.Contains(s, "# my stack") {
		t.Errorf("other content changed:\n%s", s)
	}
	if got, _ := FindImage(p, "web"); got != "nginx:1.27.2" {
		t.Errorf("re-read = %q", got)
	}
}

func TestSetImage_QuotedValue(t *testing.T) {
	p := write(t, sample)
	if err := SetImage(p, "db", "postgres:17-alpine"); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(p)
	if !strings.Contains(string(out), `image: "postgres:17-alpine"`) {
		t.Errorf("quoted value not rewritten:\n%s", out)
	}
}

func TestFileFor_Dir(t *testing.T) {
	p := write(t, sample)
	dir := filepath.Dir(p)
	if got, err := FileFor(dir); err != nil || got != p {
		t.Errorf("FileFor(dir) = %q, %v", got, err)
	}
}
