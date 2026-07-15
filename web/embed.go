package web

import "embed"

//go:embed index.html app.js style.css bticino-logo.svg
var Files embed.FS
