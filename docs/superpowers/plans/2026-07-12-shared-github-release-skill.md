# Shared GitHub Release Skill Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expose the existing Claude Code GitHub release command as a Codex project skill while keeping one physical instruction file.

**Architecture:** `.claude/commands/github-release.md` remains the canonical file and gains Agent Skills-compatible frontmatter. `.agents/skills/github-release/SKILL.md` is a relative symbolic link to the canonical file, so Claude Code and Codex load identical instructions.

**Tech Stack:** Markdown, YAML frontmatter, POSIX symbolic links, Codex skill validation scripts.

## Global Constraints

- Every release tag must use the exact `fork/vX.Y.Z` format.
- Preserve the Claude Code `/github-release` command path.
- Store the workflow in one physical file only.
- Use platform-neutral wording; do not name a Claude-only interaction tool.
- Do not add scripts, references, assets, or UI metadata.
- Per repository testing policy, do not add content-shape tests for this documentation-only conversion; verify structure and link behavior directly.

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
