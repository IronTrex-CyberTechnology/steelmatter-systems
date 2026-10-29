// Package schemas embeds the payload schemas. Execution and observation
// accept v2 external-destination evidence as well as v1. packages/verifier
// keeps byte-identical copies, checked by the conformance suite.
package schemas

import "embed"

//go:embed *.schema.json
var FS embed.FS
