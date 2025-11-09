# Repository Guidelines

## Project Structure & Module Organization
This template starts lean so you can drop service code in quickly. Add your application modules at the repository root (for example `webapp/go` or `infra/ansible`) and keep operational tooling inside `mybin/`. Follow the existing `mybin/mysql/mysql_slow_query.sh` layout when adding helpers—group them by service and document usage in the header. Keep top-level docs (`README.md`, `AGENTS.md`) current so new teammates can orient fast.

## Build, Test, and Development Commands
Surface repeatable workflows as soon as a service lands. Use the native runner inside each service directory (`go test ./...`, `npm run build`, etc.) and record the exact command in a local README. The provided database helper toggles slow-query logging: run `./mybin/mysql/mysql_slow_query.sh init` once to seed config, then `... on` / `... off` during profiling. When workflows span services, add a `Makefile` target (`make bench`, `make setup`) so CI or on-call engineers can replay them verbatim.

## Coding Style & Naming Conventions
Shell utilities stay POSIX-compliant: begin scripts with `#!/bin/sh -eu`, use two-space indentation, and guard destructive changes with explicit backups. Name helper scripts as `<service>/<purpose>.sh` under `mybin/` for predictable tab completion. For other languages adopt their canonical formatter (`gofmt`, `prettier`, `rustfmt`) and capture shared editor defaults in `.editorconfig` once the first service appears.

## Testing Guidelines
Lint every shell script with `shellcheck mybin/mysql/mysql_slow_query.sh` (and new additions) before pushing. Keep unit and benchmark tests beside the code, using the language’s standard naming (`foo_test.go`, `foo.spec.ts`). Document performance baselines in PRs whenever a change touches request handling or database access.

## Commit & Pull Request Guidelines
History mixes Conventional Commit prefixes with concise summaries (for example, a `feat:` entry that adds `mybin` and still includes an emoji in the subject). Prefer `type: summary` (English or Japanese) plus optional emoji for clarity, and keep the subject under ~60 characters. Reference issues or benchmarks in the body, explain the failure signal, and attach logs or screenshots when behaviour changes. PR descriptions should outline impact, rollback steps, and any config touch points.

## Ops & Configuration Tips
`mybin` scripts call `sudo`, so run them from a shell with appropriate privileges and verify backups land next to the target (`mysqld.cnf.*.bak`). Track machine-specific overrides via configuration management instead of ad-hoc edits. Rotate `/var/log/mysql/mysql-slow.log` after long profiling sessions to prevent disk churn.
