# Changelog Maintenance

**Mandatory working rule for agents, linked (force-loaded) from the repo-root `CLAUDE.md`.**

`CHANGELOG.md` in the **repo root** is the user-facing "what changed". Write for users, in
plain language, not implementation detail.

## Rules

- **Maintain it as you work.** Every commit that changes what users get adds a bullet in the
  **same commit**, under **today's** `## YYYY-MM-DD` block. If that block is missing, create
  it on top. The project is date-based: no version numbers, see
  [`documentation-version-policy.md`](documentation-version-policy.md).
- **Categories**: `### Added` (new features), `### Fixed` (bug and behaviour fixes),
  `### Removed` (things taken out), `### Info` (notes worth knowing). Omit unused ones; do
  not invent others.
- **Bullet format**: `- **Short name**: one short sentence on what got better for the
  user.` Hard-wrap at about 90 columns, continuation lines indented two spaces.
- **Plain language, short**: entries must be understandable by a user with no technical
  knowledge. Say what got better for the user, never how. No file names, function names,
  flags, libraries, protocols, config keys, commit ids or implementation details. Drop
  anything a non-technical user would not notice. Example:
  `- **Variable refresh rate**: fixed an issue with variable refresh rate.`
- **One block per calendar day, newest on top.** Never change a past block's date or
  rewrite past entries.
- **Skip internal-only changes**: docs, refactors, tests, CI and build tweaks with no
  user-visible effect add nothing, and a day of only such work adds no block.
- **Anonymize**: no hostnames, usernames, emails, real names, absolute home paths or
  private project names.
