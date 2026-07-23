// Package web holds Belay's embedded UI assets (templates + static files).
package web

import "embed"

//go:embed templates static
var FS embed.FS
