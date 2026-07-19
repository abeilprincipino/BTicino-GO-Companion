package webui

import "embed"

//go:embed static/index.html static/app.js static/style.css static/logo.svg
var staticFiles embed.FS
