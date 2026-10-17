// Package docs embeds the machine-readable API description that the hub
// serves at /docs/openapi.yaml and renders at /docs.
package docs

import _ "embed"

// OpenAPI is docs/openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPI []byte
