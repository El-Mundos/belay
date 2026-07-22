// Package compose locates compose files and rewrites a service's image tag in place,
// preserving formatting (only the exact image line is touched), with optional git commit.
package compose

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

var candidates = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// FileFor resolves a project path (a compose file, or a directory containing one) to a file path.
func FileFor(project string) (string, error) {
	info, err := os.Stat(project)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return project, nil
	}
	for _, name := range candidates {
		p := filepath.Join(project, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no compose file found in %s", project)
}

// FindImage returns the image ref of a service.
func FindImage(file, service string) (string, error) {
	n, err := imageNode(file, service)
	if err != nil {
		return "", err
	}
	return n.Value, nil
}

// SetImage rewrites the image tag of a service in place, editing only the image line so all other
// formatting (comments, indentation, quoting) is preserved.
func SetImage(file, service, newImage string) error {
	n, err := imageNode(file, service)
	if err != nil {
		return err
	}
	if n.Value == newImage {
		return nil
	}
	return replaceOnLine(file, n.Line, n.Value, newImage)
}

// Service is a compose service and its pinned image (empty if the service builds instead of pulling).
type Service struct {
	Name  string
	Image string
}

// Services lists all services in the compose file with their image refs, in file order.
func Services(file string) ([]Service, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	services := mapValue(root, "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return nil, nil
	}
	var out []Service
	for i := 0; i+1 < len(services.Content); i += 2 {
		name := services.Content[i].Value
		img := ""
		if n := mapValue(services.Content[i+1], "image"); n != nil {
			img = n.Value
		}
		out = append(out, Service{Name: name, Image: img})
	}
	return out, nil
}

func imageNode(file, service string) (*yaml.Node, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	svc := mapValue(mapValue(root, "services"), service)
	if svc == nil {
		return nil, fmt.Errorf("service %q not found in %s", service, file)
	}
	img := mapValue(svc, "image")
	if img == nil {
		return nil, fmt.Errorf("service %q has no image: in %s", service, file)
	}
	return img, nil
}

// mapValue returns the value node for key within a mapping node, or nil.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// replaceOnLine replaces the first occurrence of old with new on the given 1-based line.
func replaceOnLine(file string, line int, old, newVal string) error {
	info, err := os.Stat(file)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	lines := strings.Split(string(b), "\n")
	if line < 1 || line > len(lines) {
		return fmt.Errorf("image line %d out of range in %s", line, file)
	}
	i := line - 1
	if !strings.Contains(lines[i], old) {
		return fmt.Errorf("expected %q on line %d of %s, not found", old, line, file)
	}
	lines[i] = strings.Replace(lines[i], old, newVal, 1)
	return os.WriteFile(file, []byte(strings.Join(lines, "\n")), info.Mode())
}

// CommitIfRepo commits the compose file if its directory is a git repo; otherwise it's a no-op.
func CommitIfRepo(file, msg string) error {
	dir := filepath.Dir(file)
	if exec.Command("git", "-C", dir, "rev-parse", "--git-dir").Run() != nil {
		return nil // not a git repo
	}
	if err := exec.Command("git", "-C", dir, "add", filepath.Base(file)).Run(); err != nil {
		return err
	}
	if exec.Command("git", "-C", dir, "diff", "--cached", "--quiet").Run() == nil {
		return nil // nothing staged
	}
	return exec.Command("git", "-C", dir, "commit", "-m", msg).Run()
}
