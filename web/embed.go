// Package webassets embeds the default templates and static files.
// Files under PAYMUX_DATA_DIR/custom/ override these at runtime.
package webassets

import "embed"

// FS holds templates/ and static/.
//
//go:embed templates static
var FS embed.FS
