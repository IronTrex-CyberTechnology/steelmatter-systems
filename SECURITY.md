# Security Policy

## Supported versions

Steelmatter R0 is a developer reference and source preview for local evaluation with synthetic purchase-order workflows. It is not a production-ready deployment. We welcome reports affecting the current R0 source on the `main` branch. No production or security-update support is promised for R0 or earlier snapshots. Future releases are not covered by this policy unless their documentation says otherwise.

## Reporting a vulnerability

Please report suspected vulnerabilities privately. Do not include vulnerability details, proof-of-concept code, credentials, or sensitive data in a public issue, pull request, or discussion.

1. Visit the repository's [security advisories page](https://github.com/IronTrex-CyberTechnology/steelmatter-systems/security/advisories). If **Report a vulnerability** is available, use it to submit a private report.
2. If that option is unavailable, open a [public issue](https://github.com/IronTrex-CyberTechnology/steelmatter-systems/issues/new) asking maintainers for a private reporting channel. Do not disclose the vulnerability or other sensitive details in that issue. Wait for a maintainer to provide a private channel before sharing them.

A useful private report includes the affected version or commit, relevant configuration, a description of the issue and potential impact, and minimal steps or a proof of concept needed to reproduce it. Use synthetic data and redact secrets and personal information. Tell us how we can follow up with you privately.

Maintainers will assess reports and may request additional information. Response and remediation times depend on the report and available resources; no specific deadline is promised. Please do not submit a public pull request containing a fix for an undisclosed vulnerability; coordinate a fix privately with the maintainers first.

## Testing boundaries

Test only a local copy or systems you own or are explicitly authorized to test, using synthetic data. The public repository is permission to inspect its source; it is not authorization to test IronTrex's hosted systems, other people's deployments, third-party services, or real financial or procurement systems.

Avoid accessing or changing other people's data, disrupting services, excessive traffic or denial-of-service testing, social engineering, and attempts to obtain credentials. Stop if you encounter real data or an environment outside your authorization, and do not retain or share that data.

## Coordinated disclosure

Please keep vulnerability details and any fix private while maintainers investigate and coordinate disclosure with you. Discuss the scope and timing of any public advisory with maintainers; this policy does not set a fixed disclosure deadline. If a report is accepted, maintainers can work with the reporter on an appropriate fix or mitigation and disclosure. Credit can be discussed as part of that process.

## Preview scope and limitations

R0 demonstrates bounded authority, exact-action admission, duplicate prevention against a simulated destination, and signed evidence in a local synthetic workflow. These demonstrations and their recorded tests are scoped to the documented reference configuration; they are not a guarantee of security in every environment. The observer is a separated readback, not independent attestation: it shares host and database administrators with the service.

R0 does not provide live banking, payment, or procurement integration. The reference uses one organisation and INR; the agent is a deterministic stand-in and the browser demo is recorded playback. Later-release cases and capabilities remain outside the R0 scope. See the [README](README.md), [release notes](RELEASE_NOTES.md), and [evidence documentation](docs/evidence.md) for the implementation, measured checks, and limitations.
