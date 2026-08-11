package admin

import "embed"

//go:embed templates/*.html
var templates embed.FS

//go:embed static/dashboard.js
var dashboardJS []byte
