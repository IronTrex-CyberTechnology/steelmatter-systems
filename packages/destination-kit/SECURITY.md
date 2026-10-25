# Security boundary of the destination kit

This is an experimental authoring/test package, not a production enforcement
boundary. Do not connect the example adapter or conformance factory to real
resources. No payment execution is supported.

- A syntactically valid manifest is a set of unverified declarations. It does
  not prove that an API implements conditional writes, deduplication or recovery.
- Bound actions are unsigned content snapshots. Callers can recompute their
  hashes. There is no permit verification, identity authentication, entitlement
  lookup, revocation check, delegation tree or authority ledger in this kit.
- The manifest fingerprint does not pin executable code, credentials or endpoint
  configuration. Production deployment would need separate trusted registration
  and code/manifest provenance controls. Agents must not select arbitrary
  implementations, destinations, observers or credentials.
- Precheck is advisory. A read followed by an unconditional write is not atomic
  check-and-change. Reject unsupported destinations instead of overstating safety.
- Timeouts and unavailable/missing journal reads are unknown outcomes. Never
  release capacity or issue a new operation ID merely because a call timed out.
  The demo makes no retries. Durable recovery and capacity holds are not provided.
- The sample's state and journal disappear on restart; they are not thread-safe
  across processes. Its synchronous section only models local atomicity.
- Observation comparison checks data, not authority, signatures, freshness or
  independent infrastructure. The example's read path shares process and state
  with its writer. A malicious adapter can fabricate matching observations.
- Direct object APIs expect plain local data. JavaScript Proxies can execute
  traps during introspection; never evaluate untrusted code to produce input.
  Parse untrusted UTF-8 bytes with `parseManifest` instead.
- The CLI has no execute command, network fetch, credential loader, dynamic
  plugin loader or filesystem output. Scaffolds are unimplemented by default.
- Conformance hooks execute caller-supplied code and intentionally mutate the
  fixture. A `synthetic` manifest is not a sandbox: review the fixture first.

Do not include credentials, customer data or exploitable private deployment
details in public issues. Report a minimal synthetic reproducer where possible.
