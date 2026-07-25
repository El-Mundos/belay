// Package discover finds Docker Compose projects on the local daemon by reading the labels Compose
// stamps on every container it creates. This lets Belay auto-manage what's actually running,
// instead of being hand-fed compose file paths.
package discover

import (
	"context"
	"os/exec"
	"sort"
	"strings"
)

// Project is a discovered compose project: its name and the compose file that defines it.
type Project struct {
	Name string
	File string
}

// RunningProjects returns the compose projects with running containers on the local Docker daemon,
// de-duplicated by project name, ordered by name.
func RunningProjects(ctx context.Context) ([]Project, error) {
	const format = `{{.Label "com.docker.compose.project"}}` + "\t" +
		`{{.Label "com.docker.compose.project.config_files"}}` + "\t" +
		`{{.Label "com.docker.compose.project.working_dir"}}`
	out, err := exec.CommandContext(ctx, "docker", "ps", "--format", format).Output()
	if err != nil {
		return nil, err
	}
	seen := map[string]Project{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		name := f[0]
		if name == "" {
			continue // not a compose-managed container
		}
		if _, ok := seen[name]; ok {
			continue
		}
		file := ""
		if len(f) > 1 && f[1] != "" {
			file = strings.SplitN(f[1], ",", 2)[0] // config_files is comma-separated; take the first
		} else if len(f) > 2 && f[2] != "" {
			file = strings.TrimRight(f[2], "/") + "/docker-compose.yml"
		}
		if file == "" {
			continue
		}
		seen[name] = Project{Name: name, File: file}
	}
	projects := make([]Project, 0, len(seen))
	for _, p := range seen {
		projects = append(projects, p)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
	return projects, nil
}
