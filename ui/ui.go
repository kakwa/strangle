// Package ui embeds the built web UI into the strangle binary.
//
// The dist directory is populated by `make ui` (a Vite build). A placeholder
// dist/index.html is committed so that a plain `go build` works on a fresh
// checkout; building through the Makefile embeds the real UI.
package ui

import "embed"

// FS holds the built web UI, rooted at ui/dist.
//
//go:embed all:dist
var FS embed.FS
