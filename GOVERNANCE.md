# Governance

Imatra is governed in a transparent, accessible way for the benefit of its
community. Participation is open to anyone and is not bound to the
participant's employer. Everyone taking part follows the
[Code of Conduct](CODE_OF_CONDUCT.md).

## Stewardship

The project is stewarded by Salute Solutions Oy (Chargetronic), which started
it and holds its name. This file is the project's only governance document.
If the project moves to a foundation later, the foundation's charter will
replace the parts of this file it covers.

## Roles

### Contributor

Anyone who submits content to the project: code, documentation, tests,
issues, reviews.

How to become one:

* read [CONTRIBUTING.md](CONTRIBUTING.md);
* open a pull request whose commits carry a DCO sign-off;
* have it approved by a committer and merged.

### Committer

A contributor who may merge into the repository and is responsible for the
health of the code they look after.

Responsibilities:

* triage issues and review pull requests in their areas
  ([CODEOWNERS](.github/CODEOWNERS)) in reasonable time;
* keep open pull requests moving or close them with a reason;
* follow the project's channels (delayed answers are fine);
* keep the checks in CI green and the contracts stable (see below).

How to become one:

* show experience with the code through contributions and reviews;
* open a pull request that adds you to [COMMITTERS.csv](COMMITTERS.csv);
* it is merged after a majority of the current committers approve it in the
  pull request. That pull request is the record of the vote. The lead then
  grants write access.

A committer who no longer can or wants to do the work moves to emeritus
status, at their request or, in exceptional cases, by a vote of the
committers. Write access goes with the role: it is removed when a committer
becomes emeritus.

### Lead

The committers elect a lead (and optionally a co-lead) by simple majority.
The lead is the project's primary contact, is responsible for the overall
health and direction of the project, and coordinates releases and work with
other projects. The committers together make the project's technical
decisions (see Decisions).

## Current leaders

| Role | Name | GitHub |
|---|---|---|
| Lead, committer | Dmitrii Ershov (Chargetronic) | [@DmitriiE](https://github.com/DmitriiE) |

All committers and emeritus committers: [COMMITTERS.csv](COMMITTERS.csv).

## Decisions

* Most decisions are made in pull requests and issues, by lazy consensus: a
  proposal stands unless a committer objects within five working days.
* A change to a **contract** — the device protocol, the registry and its
  rules, the database schema, `/v1`, event types, settings, metrics — needs
  the approval of two committers once the project has two or more, and an
  entry in [CHANGELOG.md](CHANGELOG.md). Larger architectural decisions are
  recorded as ADRs in [docs/adr/](docs/adr/).
* If consensus is not reached, the committers vote. Each committer has one
  vote; a simple majority of those voting decides, with a quorum of more than
  half of the committers. Votes on roles need a majority of all committers.
* Disputes are first worked out between the people involved; the committers
  are called in only if that fails.

## Releases

How releases are made and versioned: [RELEASE.md](RELEASE.md). What the
project plans and does not plan: [ROADMAP.md](ROADMAP.md).

## Meetings

There are no public meetings yet. From the first public release the project
plans a monthly public technical call. Its schedule and joining instructions,
the agendas (issues labelled `meeting`) and the notes will be linked here.

## Communication

The project is a global community. It keeps discussion on open channels
(issues, pull requests and the channels listed in
[SUPPORT.md](SUPPORT.md)), uses English, and respects time zones and language
differences when scheduling and answering.

## Security

Every person with write access must have two-factor authentication enabled on
their GitHub account. Vulnerabilities are handled as described in
[SECURITY.md](SECURITY.md).
