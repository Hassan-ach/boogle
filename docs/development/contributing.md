# Contributing

There is no `CONTRIBUTING.md` in the repository. The rules below are the
**observed** conventions from commit history, branch names, and CI workflows;
they are descriptions, not an enforced policy. Where a written rule exists (for
example, the CI checks), it is called out explicitly.

## Branches and commits

Recent history uses two long-lived integration branches, `main` and `dev`, with
merge commits for feature branches. Observed branch names follow an
`<area>/<topic>` shape, for example `spider/clean`, `feat/cd`, `opt/docker`, and
`docs/update-service-docs`.

Commit messages follow Conventional Commits:

```text
<type>(<scope>): <short imperative description>
```

Types seen in history include `feat`, `fix`, `docs`, `test`, `chore`,
`refactor`, and `perf`. Examples:

```text
fix(indexer): reconnect the consumer instead of exiting on a broker blip
test(ranking): cover reciprocal edges and stop mutating os.environ
docs: align architecture, spider schema, indexer state and ranking triggers
chore(just): add a one-stop run recipe for the dev stack
```

Scope is usually the service name when the change is service-specific. Older
commits (`docker(engine) add entrypoint script`, `add cd workflow`) predate the
current style; do not copy them.

!!! note "No branch-protection rule was verified"
    The workflows show that PRs target `main` and `dev`, but no repository
    settings were inspected, so required reviews or status checks cannot be
    confirmed here.

## Before opening a PR

1. **Keep changes in the owning service.** Root recipes fan out, but the root
   `justfile` does not own service source. Cross-service changes need a clear
   description in the PR body.
2. **Update the service docs.** Service behavior, configuration keys, and
   message-contract changes belong in `services/<svc>/docs/`. See
   [Documentation](documentation.md).
3. **Run the fast tests:**

   ```bash
   just test
   ```

   This runs engine and spider Go tests, `cargo test`, and ranking pytest
   without infrastructure. See [Testing](testing.md) for the integration tier,
   which is not run in CI.

4. **Run docs validation if any Markdown, the nav, or `scripts/docs.py`
   changed:**

   ```bash
   just docs-check
   ```

   This aggregates service docs, verifies that every service page is in
   `mkdocs.yml` nav, and builds the whole site with `--strict`.

5. **Do not commit generated or local files.** `.gitignore` covers `.env`,
   `.env.docker` (including service overlays), `venv/`, `target/`,
   `node_modules/`, `public/`, `*_templ.go`, `site/`, `site-services/`,
   `docs/services/`, `docs/assets/`, `coverage/`, and `*.out` (including
   `coverage.out`). It does **not** ignore arbitrary `.env.local`-style files.
   If a generated path shows up as untracked, fix the ignore rule rather than
   committing the file.

6. **Keep secrets out of the diff.** Credentials belong in gitignored `.env`
   files or a secret manager. Never paste a DSN, token, or password into docs,
   logs, tests, or a PR description.

## Formatting and static checks

Run the checks that CI will run:

| Change | Local command | Enforced by |
|---|---|---|
| Go (engine, spider) | `gofmt -l .` and `go vet ./...` inside the service | service CI workflows |
| Rust (indexer) | `cargo fmt --check` | indexer CI |
| Shell scripts | `shellcheck scripts/*.sh services/*/entrypoint.sh` | infra CI |
| Python (ranking) | `just test-cov` / pytest with `--cov-fail-under=80` | ranking CI |
| Docs | `just docs-check` | docs CI |

`golangci-lint` and `cargo clippy` are **not** CI gates; both have known debt.
See [Coding conventions](coding-conventions.md) and
[Known issues](../reference/known-issues.md).

## CI behavior

Per-service workflows are path-filtered: changes under `services/engine/**`
trigger engine CI, and so on. Infrastructure changes trigger `infra_ci.yaml`,
and docs changes trigger `docs_ci.yaml`. Pull requests against `main` or `dev`
run these checks; **no integration test runs in CI**.

Tag pushes publish images to GHCR and do not run tests:

| Tag pattern | Image |
|---|---|
| `engine@v*` | engine |
| `spider@v*` | spider |
| `indexer@v*` | indexer |
| `ranking@v*` | ranking |

There is no deploy job in the workflows — publication is the final automated
step. See [Environment setup](setup.md) for how to reproduce the build locally.

## PR description

Include what changed and why, which service(s) are affected, the commands you
ran, and whether integration tests were exercised locally. Link the relevant
service docs page when behavior changed. A short test checklist is enough:

```markdown
## Summary
- <what changed and why>

## Test plan
- [ ] just test
- [ ] just docs-check (docs changes only)
- [ ] just test-integration (optional, needs local Postgres)
```

## Related pages

- [Coding conventions](coding-conventions.md)
- [Testing](testing.md)
- [Documentation](documentation.md)
- [Commands reference](../reference/commands.md)
