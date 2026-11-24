// Package web embeds the built web UI into the strangle binary.
//
// The dist directory is populated by `make web` (a Vite build). A placeholder
// dist/index.html is committed so that a plain `go build` works on a fresh
// checkout; building through the Makefile embeds the real UI.
package web

import "embed"

// FS holds the built web UI, rooted at web/dist.
//
//go:embed all:dist
var FS embed.FS
