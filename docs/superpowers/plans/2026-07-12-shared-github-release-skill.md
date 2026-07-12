# Shared GitHub Release Skill Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Share one GitHub release workflow between Claude Code and Codex, and automatically infer and apply the next semantic version.

**Architecture:** `.claude/commands/github-release.md` remains the canonical file and `.agents/skills/github-release/SKILL.md` remains a relative symbolic link. The canonical workflow classifies commit subjects and bodies using ordered semantic rules, defaults unknown messages to `patch`, computes the next `fork/vX.Y.Z` tag, and proceeds without an interactive version choice.

**Tech Stack:** Markdown, YAML frontmatter, POSIX symbolic links, Codex skill validation scripts.

## Global Constraints

- Every release tag must use the exact `fork/vX.Y.Z` format.
- Preserve the Claude Code `/github-release` command path.
- Store the workflow in one physical file only.
- Use platform-neutral wording; do not name a Claude-only interaction tool.
- Do not add scripts, references, assets, or UI metadata.
- Do not add brittle source-shape tests. Verify the public workflow with read-only classification scenarios plus structure and link checks.

---

### Task 1: Share the release workflow between Claude Code and Codex

**Files:**
- Modify: `.claude/commands/github-release.md`
- Create: `.agents/skills/github-release/SKILL.md` as a symbolic link

**Interfaces:**
- Consumes: Claude Code custom command discovery at `.claude/commands/github-release.md`
- Produces: Codex project skill discovery at `.agents/skills/github-release/SKILL.md`

- [ ] **Step 1: Add Agent Skills frontmatter to the canonical command**

Add this frontmatter before the existing heading:

```yaml
---
name: github-release
description: 用于 Go 项目发版、根据 Conventional Commits 计算语义化版本、创建或回滚 fork/v* Tag，以及通过 GitHub Actions 触发自动发布。
---
```

- [ ] **Step 2: Remove the Claude-only interaction reference**

Change the non-conventional commit rule from:

```markdown
| 不符合规范 | 询问用户 | `AskUserQuestion` |
```

to:

```markdown
| 不符合规范 | 停止并询问用户应升级 `major`、`minor` 还是 `patch` | 不要猜测 |
```

- [ ] **Step 3: Create the Codex skill link**

Run from the repository root:

```bash
mkdir -p .agents/skills/github-release
ln -s ../../../.claude/commands/github-release.md .agents/skills/github-release/SKILL.md
```

Expected: `SKILL.md` is a relative symbolic link and the canonical Markdown file remains the only physical copy.

- [ ] **Step 4: Validate the shared skill**

Run:

```bash
test "$(readlink .agents/skills/github-release/SKILL.md)" = "../../../.claude/commands/github-release.md"
cmp -s .claude/commands/github-release.md .agents/skills/github-release/SKILL.md
python3 /Users/caidaoli/.codex/skills/.system/skill-creator/scripts/quick_validate.py .agents/skills/github-release
```

Expected: every command exits with status `0`; the validator prints a success message.

- [ ] **Step 5: Inspect final repository state**

Run:

```bash
git status --short --ignored .claude/commands/github-release.md .agents/skills/github-release/SKILL.md
```

Expected: both local agent paths may remain ignored by the repository, but the files exist and resolve correctly. Do not alter `.gitignore`.

### Task 2: Infer and apply the release version automatically

**Files:**
- Modify: `.claude/commands/github-release.md`
- Verify through link: `.agents/skills/github-release/SKILL.md`

**Interfaces:**
- Consumes: commit subjects and bodies after the latest `fork/v*` tag
- Produces: one automatically selected `major`, `minor`, or `patch` level and the resulting `fork/vX.Y.Z` tag

- [ ] **Step 1: Record the current behavior gap with representative commit sets**

Use the current Skill to classify these read-only scenarios and report whether user input is required:

```text
Previous tag: fork/v8.11.0
A: add support for S3-compatible storage
B: update dependencies
C: fix(api): reject malformed requests + feat(api): add batch endpoint
D: feat(api): replace legacy response schema; body contains BREAKING CHANGE: clients must use items
```

Expected gap: the current rule requires user input for A and B instead of inferring `minor` and `patch`.

- [ ] **Step 2: Read enough commit data for semantic inference**

Replace the subject-only command with:

```bash
COMMIT_COUNT=$(git rev-list --count "${PREV_TAG}..HEAD")
git log "${PREV_TAG}..HEAD" --no-merges --pretty=format:'%H%n%s%n%b%n---'
```

If `COMMIT_COUNT` is `0`, stop because there is no release content.

- [ ] **Step 3: Replace interactive classification with ordered inference**

Document these exact rules:

```text
1. major: type!:, BREAKING CHANGE:, or an explicit incompatible public API, protocol, configuration, CLI, or persisted-data change.
2. minor: feat: or an explicit new externally visible capability expressed as add, introduce, support, or enable.
3. patch: fixes, performance, refactors, docs, tests, dependencies, build, CI, internal-only work, and every unrecognized message.
4. Mixed commits use the highest level: major > minor > patch.
5. Merge commit titles never determine the level.
6. Internal helpers, tests, and documentation containing words such as add or support remain patch.
```

Require the agent to print the selected level, decisive commit, and new tag, then continue without asking the user to choose a version.

Compute the new version exactly as follows:

```text
major: X.Y.Z -> (X+1).0.0
minor: X.Y.Z -> X.(Y+1).0
patch: X.Y.Z -> X.Y.(Z+1)
NEW_TAG: fork/v<computed-version>
```

- [ ] **Step 4: Guard tag creation**

Before creating the tag, require this check:

```bash
git rev-parse -q --verify "refs/tags/${NEW_TAG}"
```

If it succeeds, stop because the target tag already exists. Otherwise create and push `NEW_TAG`.

- [ ] **Step 5: Re-run the representative scenarios**

Expected classifications:

```text
A -> minor, no question
B -> patch, no question
C -> minor, no question
D -> major, no question
```

- [ ] **Step 6: Validate the shared Skill**

Run:

```bash
test "$(readlink .agents/skills/github-release/SKILL.md)" = "../../../.claude/commands/github-release.md"
cmp -s .claude/commands/github-release.md .agents/skills/github-release/SKILL.md
uv run --no-project --with pyyaml python /Users/caidaoli/.codex/skills/.system/skill-creator/scripts/quick_validate.py .agents/skills/github-release
```

Expected: link and content checks exit `0`; the validator prints `Skill is valid!`.
