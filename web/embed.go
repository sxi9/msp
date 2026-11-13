// Package web embeds the dashboard bundle so that msp-server is one static
// binary. web/dist/index.html is committed and always present; dist/app.js and
// dist/app.css appear after `make web` (esbuild) and are picked up by the next
// `go build`. Without them the committed index.html tells the operator to run
// `make web`, so go build and every Go test work without Node.
package web

import "embed"

// FS holds dist/index.html plus, when built, dist/app.js and dist/app.css.
// The server serves it with index.html as the fallback for any path outside
// /api and /dl, with Cache-Control: no-cache.
//
//go:embed dist/*
var FS embed.FS
