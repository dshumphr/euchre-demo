// Package webassets embeds the euchre web frontend: the repo-root web/
// directory (index.html, style.css, app.js), which the frontend worker owns.
// go:embed patterns are package-relative, so the embed lives at the module
// root — one level above web/ — and cmd/euchre-web imports it. The files are
// embedded exactly as committed; nothing copies or rewrites them.
package webassets

import "embed"

// Web is the frontend filesystem, rooted at the web/ directory.
//
//go:embed all:web
var Web embed.FS
