# Shared GitHub Release Skill Design

## Goal

Make the existing Claude Code `/github-release` command available to Codex as a project skill without duplicate instructions, and make semantic-version selection automatic for recognizable and unrecognized commit messages.

## Root Cause

Claude Code and Codex discover reusable instructions through different paths, so copying the command would create two sources of truth. The existing version rule also equates non-Conventional Commit messages with ambiguity even when their intent is inferable, causing unnecessary user prompts.

## Design

- Keep `.claude/commands/github-release.md` as the only physical instruction file.
- Add Agent Skills-compatible `name` and `description` YAML frontmatter to that file.
- Create `.agents/skills/github-release/SKILL.md` as a relative symbolic link to the Claude command.
- Keep the existing `fork/v*` tag requirement and rollback workflow unchanged.
- Replace Claude-specific tool wording with platform-neutral instructions so both agents can execute the same workflow.
- Do not add scripts, references, assets, or UI metadata; the workflow is small and self-contained.

## Automatic Version Inference

- Read commit subjects and bodies after the latest `fork/v*` tag while excluding merge commits from classification.
- Select `major` when any commit contains an explicit breaking marker such as `type!:` or `BREAKING CHANGE:`, or clearly describes an incompatible public behavior or protocol change.
- Otherwise select `minor` when any commit uses `feat:` or clearly describes adding, introducing, enabling, or supporting a new capability.
- Classify fixes, performance work, refactors, documentation, tests, dependencies, build changes, CI changes, and unrecognized messages as `patch`.
- Select the highest inferred level across all commits: `major` over `minor` over `patch`.
- Print the selected level, decisive commit evidence, and resulting `fork/vX.Y.Z` tag, then continue with tag creation and push without asking the user to choose a version.
- Stop instead of guessing when there are no commits to release, the target tag already exists, or repository state prevents a safe release.

## Compatibility

- Claude Code continues to expose `/github-release` through the existing command path.
- Codex discovers `github-release` through the project-level `.agents/skills` path.
- The relative link keeps the setup portable when the repository is moved.

## Verification

- Confirm the symbolic link resolves to the Claude command.
- Confirm both paths return identical content.
- Run the Codex skill validator against `.agents/skills/github-release`.
- Inspect the final files to ensure the `fork/v*` rule remains explicit.
- Check representative commit sets for breaking, feature, patch-only, mixed, and unrecognized-message classification.

## Scope

This change shares the release workflow and replaces interactive version selection with automatic inference. It does not execute a release or alter Git configuration.
