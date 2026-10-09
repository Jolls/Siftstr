// Package web holds the embedded static assets and HTML templates.
package web

import "embed"

//go:embed static templates
var FS embed.FS
