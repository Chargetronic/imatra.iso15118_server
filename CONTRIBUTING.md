# Contributing

Thank you for your interest in contributing. This document explains how to
propose a change and what a change must meet. The roles in the project
(contributors, committers, lead) are described in [GOVERNANCE.md](GOVERNANCE.md).
For help or questions see [SUPPORT.md](SUPPORT.md). Security vulnerabilities
are never reported in public: follow [SECURITY.md](SECURITY.md).

## How to contribute a fix or a change

1. For anything larger than a small fix, open an issue first and describe the
   problem and the proposed approach; a change to a contract (see below) needs
   agreement in the issue before the pull request.
2. Fork the repository, create a branch, make the change with tests and
   documentation.
3. Sign off every commit (DCO, below).
4. Open a pull request against `main` using the template. CI must be green.
5. A committer reviews it (see [CODEOWNERS](.github/CODEOWNERS)) and either
   merges it or asks for changes.

## Developer Certificate of Origin

The project uses the [Developer Certificate of Origin 1.1](https://developercertificate.org/)
instead of a CLA. Every commit carries a line

```
Signed-off-by: Your Name <your.email@example.org>
```

with your real name (the name you are known by in the community) — added by
`git commit -s`. By signing off you certify that you have the right to submit
the change under the project's licence. Pull requests with commits without a
sign-off are not merged; CI checks it.

To add a missing sign-off to the last commit: `git commit --amend -s`; to a
series: `git rebase --signoff main`.

## Contributions made with AI assistants

You may use AI assistants to write code, tests or documentation. The person
who signs off the commit is its author: by the sign-off they certify the DCO
for the whole change, have reviewed and understood it, and answer for it in
review. An assistant is never the author or the signer of a commit. Naming
it in a `Co-Authored-By:` trailer is welcome but not required. Do not paste
other people's code, or confidential material, into an assistant to produce
a contribution.

## Licence and file headers

The project is licensed under the [Apache License 2.0](LICENSE); contributions
are accepted under the same licence. Every source file starts with an SPDX
header in its comment syntax:

```
// SPDX-License-Identifier: Apache-2.0
// Copyright Contributors to the project. See NOTICE.
```

`scripts/check-headers.sh` (CI) checks it. Code taken from elsewhere keeps its
original licence and copyright notices; add it to [THIRD_PARTY.md](THIRD_PARTY.md).
A new dependency needs a licence compatible with Apache-2.0 and an entry in
THIRD_PARTY.md.

## Contracts

The device protocol (`dev/<id>/…`), the registry and its business rules, the
database schema, `/v1` and its OpenAPI document, the event types and their
JSON Schema, the settings and the metrics are **contracts**: operators,
devices and other systems depend on them.

* Prefer additive changes: a new field, a new event type, a new rule key.
* A change that is not additive needs an issue, the approval of two
  committers and a **contract** entry in [CHANGELOG.md](CHANGELOG.md).
* Database migrations are new files in `api/migrations`; a released migration
  is never edited.
* Names in contracts and code are functional only: no product, brand or
  server-edition names (`scripts/check-names.sh`).

## Development

```sh
cp .env.example .env
docker compose up -d --build
```

Engine (Go 1.27, cgo with [libcbv2g](https://github.com/EVerest/libcbv2g)
v0.3.2 — see `engine/Dockerfile` or `.github/workflows/ci.yml` for the build):

```sh
cd engine
gofmt -l .                 # must print nothing
go vet ./...
VALKEY_TEST_ADDR=127.0.0.1:6379 PG_TEST_DSN='host=127.0.0.1 user=postgres password=test sslmode=disable' \
  go test -count=1 ./...   # the Valkey and Postgres tests must run, not be skipped
```

API (Python 3.12): `pip install -r api/requirements.txt`, then
`python -m compileall -q api/app`; tests are in `api/tests`.

Repository checks: `scripts/check-names.sh`, `scripts/check-headers.sh`.

## Coding style

* Go: `gofmt`, `go vet`; small packages under `engine/internal`; errors
  returned, not logged and swallowed; nothing on the path of an answer to the
  EV may wait on Postgres (see [ADR-002](docs/adr/adr-002-working-state-in-valkey.md)).
* Python: PEP 8, type hints; SQL with parameters only.
* Comments and documentation in English; explain *why*, not *what*.
* Every behaviour change comes with a test; every new setting or metric with
  documentation (`.env.example`, [docs/metrics.md](docs/metrics.md)).
* Never commit secrets, keys or real host names; CI runs a secret scan on the
  whole history.

## Commit messages

A short summary line (up to ~72 characters) saying what changes and where,
a blank line, then the reason and anything a reviewer should know. Reference
the issue (`Fixes #123`).
