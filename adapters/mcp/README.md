# @irontrex/steelmatter-mcp

Thin MCP (Model Context Protocol) adapter for the Steelmatter R0 procurement
reference. It lets an agent act as **one assigned actor** over stdio, calling
the API through [`@irontrex/steelmatter-sdk`](../../packages/sdk). Private
package; not published. Simulation with synthetic data only.

The adapter adds no policy. The service's deterministic gate makes every
decision; the model never sits on the admission path.

## Tools
The tool set is what the bound actor may do through the API (document 03). In
the R0 threat model the agent is the actor and may be compromised (document 04,
T32), and it must never hold owner, issuer or ledger credentials (document 02, I7).

| Tool | API operation | Notes |
|---|---|---|
| `steelmatter_evaluate_action` | `POST /v1/actions/evaluate` | Permit or a DENY with reason. Reserves allowance; repeats return the same permit. |
| `steelmatter_admit_operation` | `POST /v1/operations` | Present the returned action and `permit_jws` unchanged. Safe to repeat. No destination parameter. |
| `steelmatter_get_operation` | `GET /v1/operations/{id}` | Four status fields and the target reference. |
| `steelmatter_get_evidence` | `GET /v1/operations/{id}/evidence` | Signed records; verify offline with the standalone verifier. |
| `steelmatter_get_adapter_assurance` | `GET /v1/adapters/{id}/assurance` | Supported paths, limitations, test references. |

There are no tools to create or revoke mandates, create or amend instructions,
simulate or reconcile, and no tool accepts actor, tenant, audience or
destination fields. Input schemas are closed (`additionalProperties: false`).

## Results and errors
- Success: `structuredContent` = `{ status, created, request_id, result }`, also as JSON text.
- A service refusal (for example `ACTION_CHANGED`, `ACTOR_MISMATCH`, `PERMIT_EXPIRED`) is a tool result with
  `isError: true` and `structuredContent` = `{ error_kind: "API", error_code, status, message, request_id, retryable, operation_id? }`,
  so the agent sees the decision. A policy denial at evaluation is a normal result (`decision: "DENY"`).
- No response from the service: `error_kind: "TRANSPORT"` (with `timed_out`); a reply outside the contract: `error_kind: "RESPONSE"`.
- Arguments that fail the input schema (missing, wrong type, out of range, or undeclared, including inherited names such as
  `__proto__` or `constructor`) are tool results with `isError: true` and `structuredContent` =
  `{ error_kind: "INVALID_ARGUMENTS", message }` (MCP 2025-11-25), so the agent can correct the call. They never reach the service.
- An unknown tool name, a `tools/call` without a name, or `params` / `arguments` that are not objects (array, string, number,
  boolean or `null`) are malformed requests: JSON-RPC errors (`-32602`) under every supported revision. An absent `arguments`
  member means no arguments.
- JSON-RPC batches are accepted only when initialize negotiated MCP `2025-03-26`, the one supported revision that has them.
  Before initialize, or under `2025-06-18` / `2025-11-25`, a batch is one `-32600` Invalid Request and none of its members run.
- Retries, timeouts and redirect refusal come from the SDK (see its README).

## Configure
| Variable | Meaning |
|---|---|
| `STEELMATTER_SERVER` | Service base URL (default `http://127.0.0.1:8080`) |
| `STEELMATTER_MCP_TOKEN_FILE` | File with the actor's synthetic bearer token, mode `0600` (preferred) |
| `STEELMATTER_MCP_TOKEN` | The token itself (only if a file is not possible; MCP client configs are often shared) |
| `STEELMATTER_MCP_TIMEOUT_MS` | Optional per-attempt timeout |

Use the token of the actor the agent acts as, never an owner, issuer, observer or operator token.
The adapter refuses to start without a token, with a group/world-readable token file, or with both sources set.
The token never appears on stdout or stderr.

Example MCP client entry (stdio):
```json
{
  "mcpServers": {
    "steelmatter": {
      "command": "node",
      "args": ["/path/to/Steelmatter/adapters/mcp/dist/main.js"],
      "env": { "STEELMATTER_SERVER": "http://127.0.0.1:8080", "STEELMATTER_MCP_TOKEN_FILE": "/path/to/agent-a.token" }
    }
  }
}
```

## Build and test
```bash
scripts/test-mcp     # builds the SDK, links it, typechecks, builds and tests the adapter → .out/mcp.json
go test -tags integration,failpoints -run '^TestMCPAgentEndToEnd$' ./tests/integration   # live agent flow (needs STEELMATTER_TEST_ADMIN_URL)
```

## Protocol
JSON-RPC 2.0, newline-delimited over stdio. Methods: `initialize` (negotiates MCP 2025-11-25, 2025-06-18 or
2025-03-26), `ping`, `tools/list`, `tools/call`, notifications `initialized` and `cancelled` (aborts the
in-flight call; no response is sent). Batches are accepted. Only the `tools` capability is offered.
Implemented without a third-party MCP library (the repository requires human review before adding one).

## Not decided by the packet
Open decisions, tracked in the team's review record: tool sets for other principal kinds, network transports,
publication and packaging with the SDK. The adapter accepts any principal's token and relies on the service to
enforce what that principal may do; configure only an actor token (see Configure).
