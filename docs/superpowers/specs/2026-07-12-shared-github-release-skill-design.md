# Shared GitHub Release Skill Design

## Goal

Make the existing Claude Code `/github-release` command available to Codex as a project skill without maintaining duplicate release instructions.

## Root Cause

Claude Code and Codex discover reusable instructions through different paths. Copying the command into both paths would create two sources of truth that can drift.

## Design

- Keep `.claude/commands/github-release.md` as the only physical instruction file.
- Add Agent Skills-compatible `name` and `description` YAML frontmatter to that file.
- Create `.agents/skills/github-release/SKILL.md` as a relative symbolic link to the Claude command.
- Keep the existing `fork/v*` tag requirement and rollback workflow unchanged.
- Replace Claude-specific tool wording with platform-neutral instructions so both agents can execute the same workflow.
- Do not add scripts, references, assets, or UI metadata; the workflow is small and self-contained.

## Compatibility

- Claude Code continues to expose `/github-release` through the existing command path.
- Codex discovers `github-release` through the project-level `.agents/skills` path.
- The relative link keeps the setup portable when the repository is moved.

## Verification

- Confirm the symbolic link resolves to the Claude command.
- Confirm both paths return identical content.
- Run the Codex skill validator against `.agents/skills/github-release`.
- Inspect the final files to ensure the `fork/v*` rule remains explicit.

## Scope

This change only shares and normalizes the existing release workflow. It does not execute a release, alter Git configuration, or change release semantics.
