# CLAUDE.md

## 🤖 Welcome, Claude

This repository uses **`AGENTS.md`** as the single source of truth for:

* Coding conventions (naming, formatting, commenting, testing)
* Contribution workflows (branch prefixes, commit message style, PR templates)
* Release, CI, and dependency‑management policies
* Security reporting and governance links

> **TL;DR:** **Read `AGENTS.md` first.**
> All technical or procedural questions are answered there.

### Quick Checklist for Claude

1. **Study `AGENTS.md`**
   Make sure every automated change or suggestion respects those rules.
2. **Follow branch‑prefix and commit‑message standards**
   They drive Mergify auto‑labeling and CI gates.
3. **Never tag releases**
4. **Pass CI**
   Run `go fmt`, `goimports`, `go vet`, `staticcheck`, and `golangci‑lint` locally before opening a PR.

If you encounter conflicting guidance elsewhere, `AGENTS.md` wins.
Questions or ambiguities? Open a discussion or ping a maintainer instead of guessing.

Happy hacking!

---

## About This Project

`go-neverbounce` is a small Go SDK for the **NeverBounce API v4.2**: single-email verification
(`single/check`) and the native bulk-jobs workflow (`jobs/create`, `jobs/parse`, `jobs/start`,
`jobs/status`, `jobs/results`, `jobs/download`), plus two bounded helpers (`WaitForJob`, which only
polls `jobs/status`, and the `JobResultPages` iterator). It is a library, not an application: there
is no binary, no CLI, and no runtime configuration file.

- Module: `github.com/mrz1836/go-neverbounce` (package `neverbounce`, one flat package, one method
  per operation on `*Client`)
- Go version: 1.26 (CI runs 1.26.x)
- Runtime dependencies: none (`testify` is used only by tests and `internal/testutil`)

**Invariants that changes must preserve:**
- **No runtime dependencies.** Library code imports only the standard library, and never `log`,
  `os`, `unsafe`, `encoding/csv` or `net/http/httputil`. The `depguard` and `forbidigo` linters
  enforce this.
- **Exactly one HTTP attempt per call.** No retries, redirects, hedging, fallbacks, hidden
  pagination or follow-on calls, and `GetBody` stays nil so the standard library cannot resend a
  POST. Nothing paid or state-changing (`CheckEmail`, `CreateJob`, `ParseJob` with auto-start,
  `StartJob`) ever happens unless the caller calls that method.
- **The API key reaches only the provider or loopback.** It travels only in the JSON body, never in
  a URL or header. There is no general base URL option; `WithTestBaseURL` accepts only `https` on
  `127.0.0.0/8` or `[::1]`. The key lives only inside function values the `Client` holds.
- **No values in errors.** `Error()` holds only the operation, kind, HTTP status, API status and
  JSON field paths: never the key, email addresses, row data, URLs, header values, bodies or provider
  text. Provider messages on errors are opt-in (`WithProviderDiagnostics`).
- **No logging, globals (other than sentinel errors) or `init`, no background goroutines, and no
  environment reads** in SDK code (the default transport's `http.ProxyFromEnvironment` honors the
  standard proxy variables).
- **Downloads are streamed byte for byte.** The SDK never parses CSV, never closes the caller's
  writer and never rewrites line endings.
- **Unit tests stay offline.** The live contract probe (`live_probe_internal_test.go`, `live`
  build tag) can make billable calls and is run only by the owner, by hand, after approval. Never
  run it.
- **Synthetic data only.** Fixtures are written from scratch to the documented shape; never copy
  provider examples or commit real responses. CSV fixtures are Go string literals, never `.csv`
  files, because `.gitattributes` would rewrite their line endings.
