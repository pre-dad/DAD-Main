// Package api embeds the OpenAPI document so the server can resolve schemas
// against the contract it publishes.
package api

import _ "embed"

//go:embed openapi.json
var OpenAPI []byte
