# Security policy

## Supported versions

Security fixes go into the latest minor version; this table is updated with
every release ([RELEASE.md](RELEASE.md)).

| Version | Supported |
|---|---|
| 0.1.x | yes |

## Reporting a vulnerability

Please do **not** report vulnerabilities in public issues, discussions or pull
requests.

* Preferred: GitHub private vulnerability reporting — the **Report a
  vulnerability** button on the repository's *Security* tab.
* Or e-mail the project lead at cto@chargetronic.fi with the subject
  `SECURITY: <short description>`.

Please include what is affected (component, version or commit, configuration
such as `PKI_MODE`), how to reproduce it, and what an attacker gains. A proof
of concept against your own deployment or `fleetgen` is welcome; do not test
against deployments you do not operate.

## What happens next

| Step | Target |
|---|---|
| Acknowledgement of the report | within 5 working days |
| First assessment (accepted or declined, severity) | within 10 working days |
| Fix or mitigation for an accepted report | as fast as the severity requires; disclosure is coordinated with the reporter, by default no later than 90 days after the report |

The reporter is kept informed at least every two weeks until the report is
closed. Accepted reports get a GitHub security advisory and, where
applicable, a CVE; the release notes of the fixing release list them. With
their consent, reporters are credited.

If a report is declined, we explain why. Questions about the decision can be
sent to the same address.

## Scope

In scope: the code in this repository — the engine, relay, projector, the
operator API, the broker, PKI and gateway configuration, the fleet generator —
and the default configuration in `docker-compose.yml` and `.env.example`.

Out of scope: vulnerabilities in third-party components (report them
upstream; tell us if a deployment is exposed through our defaults), in a
service layer or device firmware not in this repository, and weaknesses that
need an operator to change the defaults against the documentation.

## Hardening notes for operators

* Keep `.env` and the `pki` volume private; the CA key there admits devices.
* Expose only 80/443 (gateway) and 8883 (devices); Docker publishes ports
  past host firewalls, so filter at the provider's firewall.
* Use operator keys with the least rights needed; rotate them.
* Back up Postgres and the `pki` volume (`scripts/export.sh`).
* The services talk to Postgres without TLS (`sslmode=disable`) and to Valkey
  without a password, on the compose network only. Do not publish their
  ports; if you move Postgres or Valkey to another host, turn TLS and
  authentication on.
* The deployment refuses to start with empty, example or short passwords and
  keys (`pki/preflight.sh`); make them with `openssl rand -hex 24`.
* Behind your own TLS proxy (`compose.behind-proxy.yml`), set
  `TRUSTED_PROXIES` to your proxy's address, so the gateway's rate limits see
  the real client.
* The review of the interfaces and defaults: `docs/security-review.md`.

## EU Cyber Resilience Act

The software is intended for use in commercial activities. Salute Solutions
Oy (Chargetronic) acts as its open-source software steward and handles
reports as described above.
