// Package schemas embeds the five payload JSON Schemas (schema_version "1").
// The files are the 0.6 packet schemas, unchanged; packages/verifier keeps a
// byte-identical copy (checked by the conformance suite).
package schemas

import "embed"

//go:embed *.schema.json
var FS embed.FS
