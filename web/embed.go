// Package webassets embeds the default templates and static files.
// Files under PAYMUX_DATA_DIR/custom/ override these at runtime.
package webassets

import "embed"

// FS holds templates/ and static/. "all:" keeps the _partial.html files, which
// a plain directory embed would silently drop.
//
//go:embed all:templates static all:themes i18n
var FS embed.FS
