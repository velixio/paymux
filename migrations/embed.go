// Package migrations embeds the SQL migrations for every supported dialect.
// Both directories must carry identical versions and names.
package migrations

import "embed"

// FS has sqlite/ and postgres/ directories of NNNN_name.sql files.
//
//go:embed sqlite/*.sql postgres/*.sql
var FS embed.FS
