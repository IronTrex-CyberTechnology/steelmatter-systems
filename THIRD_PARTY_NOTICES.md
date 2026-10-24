# Third-party components

This source distribution does not vendor the dependencies listed below. Package managers (`go mod download`, `npm ci`) retrieve them using `go.mod`, `go.sum` and the `package-lock.json` files. The listed dependencies declare permissive licences. This dependency inventory does not certify the provenance of every source contribution.

## Go module (service, CLI tools)

| Module | Version | Licence |
|---|---|---|
| github.com/jackc/pgx/v5 | v5.11.0 | MIT |
| github.com/jackc/pgpassfile | v1.0.0 | MIT |
| github.com/jackc/pgservicefile | v0.0.0-20240606120523-5a60cdf6a761 | MIT |
| github.com/jackc/puddle/v2 | v2.2.2 | MIT |
| github.com/go-jose/go-jose/v4 | v4.1.5 | Apache-2.0 |
| github.com/gowebpki/jcs | v1.0.2 | Apache-2.0 |
| github.com/santhosh-tekuri/jsonschema/v6 | v6.0.3 | Apache-2.0 |
| go.yaml.in/yaml/v3 | v3.0.5 | MIT and Apache-2.0 |
| golang.org/x/sync | v0.22.0 | BSD-3-Clause |
| golang.org/x/text | v0.41.0 | BSD-3-Clause |

## TypeScript packages

| Package | Used by | Version | Licence |
|---|---|---|---|
| ajv | verifier (runtime) | 8.20.0 | MIT |
| ajv-formats | verifier (runtime) | 3.0.1 | MIT |
| canonicalize | verifier (runtime) | 5.1.0 | Apache-2.0 |
| fast-deep-equal | verifier (runtime) | 3.1.3 | MIT |
| fast-uri | verifier (runtime) | 3.1.9 | BSD-3-Clause |
| jose | verifier (runtime) | 6.2.12 | MIT |
| json-schema-traverse | verifier (runtime) | 1.0.0 | MIT |
| require-from-string | verifier (runtime) | 2.0.2 | MIT |
| typescript | all (build only) | 7.0.2 | Apache-2.0 |
| esbuild | demo (build only) | 0.28.2 | MIT |
| @noble/hashes | demo (build only) | 2.4.0 | MIT |
| @types/node, undici-types | all (build only) | 26.6.5, 8.9.0 | MIT |

The SDK and MCP adapter have no third-party runtime dependencies. The browser demo bundle includes the verifier's runtime dependencies listed above.
