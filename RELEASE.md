# Releases

## Versions

The project follows [Semantic Versioning](https://semver.org/). Before 1.0.0
a minor version may change a contract; such changes are marked **contract**
in [CHANGELOG.md](CHANGELOG.md) with what an operator has to do. From 1.0.0 a
contract changes only in a major version, or additively.

Contracts: the device protocol (`dev/<id>/…`), the registry and its business
rules, the database schema (migrations in `api/migrations`, never edited once
released), `/v1` and its OpenAPI document, the event types and their JSON
Schema, the settings (environment variables), the metrics.

## Making a release

1. `main` is green in CI; open issues labelled `release-blocker` are closed.
2. Third-party licences re-checked (`go-licenses`, `pip-licenses`) and
   [THIRD_PARTY.md](THIRD_PARTY.md) updated.
3. [CHANGELOG.md](CHANGELOG.md): the `Unreleased` section becomes the version,
   with Breaking changes, Known issues, Key changes (including every fixed
   vulnerability with its CVE or advisory) and Other changes. Release notes
   are written for operators, not copied from `git log`.
4. A signed tag `vX.Y.Z` on `main`; a GitHub release with the notes.
5. Container images built from the tag and published with the version tag.
6. Supported versions in [SECURITY.md](SECURITY.md) updated.

The lead coordinates releases; any committer may prepare one. The cadence is
agreed by the committers; until 1.0.0, releases follow the milestones in
[ROADMAP.md](ROADMAP.md).
