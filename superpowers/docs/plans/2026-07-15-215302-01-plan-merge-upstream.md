# Merge upstream into the current branch

**Goal:** Merge the latest default branch from the official `upstream` remote into the current local `main` branch.
**Why planning is required:** The merge mutates an integration branch that already contains local commits.
**Acceptance:** Preserve the pre-merge anchor `9ed054473879855bedcd53bb992b98ed8820fcd4`; confirm the official upstream default branch and review incoming commits before merging; do not push; abort or stop on conflicts that cannot be resolved from repository semantics; finish with no unresolved conflicts, a successful required server build, and relevant tests passing.

### Outcome 1: Establish the upstream merge boundary
- Work: Fetch `upstream`, identify its default branch, and inspect the commits and file changes not present in the current branch.
- Verify: `git remote show upstream`, `git log HEAD..upstream/<branch> --oneline`, and `git diff HEAD..upstream/<branch> --stat`

### Outcome 2: Integrate upstream without losing local work
- Work: Merge the fetched upstream default branch into local `main`, preserving the three existing local commits and resolving only semantically clear conflicts.
- Verify: `git status --short --branch`, `git log --graph --oneline --decorate -n 20`, and `git merge-base --is-ancestor upstream/<branch> HEAD`

### Outcome 3: Verify the integrated repository
- Work: Run the repository-required server build and tests appropriate to the merged changes, then inspect the final merge state.
- Verify: `go build -o test-output ./cmd/server && rm test-output` and `go test ./...`
