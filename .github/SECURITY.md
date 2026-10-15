# 🔐 Security Policy

Security is a priority. We maintain a proactive stance to identify and fix vulnerabilities in **go-neverbounce**.

<br/>

## 🛠️ Supported & Maintained Versions
Any released version of **go-neverbounce** that is not marked as deprecated is actively supported and maintained.

<br/>

## 📨 Reporting a Vulnerability

If you’ve found a security issue, **please don’t open a public issue or PR**.

Instead, send a private email to:
📧 [go-neverbounce@mrz1818.com](mailto:go-neverbounce@mrz1818.com)

Include the following:

* 🕵️ Description of the issue and its impact
* 🧪 Steps to reproduce or a working PoC
* 🔧 Any known workarounds or mitigations

We welcome responsible disclosures from researchers, vendors, users, and curious tinkerers alike.

<br/>

## 📅 What to Expect

* 🧾 **Acknowledgment** within 72 hours
* 📢 **Status updates** every 5 business days
* ✅ **Resolution target** of 30 days (for confirmed vulnerabilities)

Prefer encrypted comms? Let us know in your initial email—we’ll reply with our PGP public key.
All official security responses are signed with it.

<br/>

## 🧪 Security Tooling

We regularly scan for known vulnerabilities using:

* [`govulncheck`](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck): Checks Go code and dependencies for known vulnerabilities using the Go vulnerability database.
* [`OSV-Scanner`](https://github.com/google/osv-scanner): As part of our CI (see `fortress.yml`), we check Go dependencies against the OSV.dev database.
* [`gitleaks`](https://github.com/gitleaks/gitleaks): Scans the repository for sensitive data or secrets that may have been accidentally committed to the codebase.

Want to run these yourself?

```sh
magex deps:audit
# or run nancy via the CI workflow
```

This will check your local build for known issues in Go modules and dependencies.

<br/>

## 🔏 Credentials and Personal Data

**go-neverbounce** handles two kinds of sensitive data: a NeverBounce API key and the email
addresses (and any other columns) being verified.

* **Never include a real API key or real contact data in an issue, pull request or report.**
  Describe the problem with synthetic values (addresses under `example.com`, keys such as
  `secret_example0000`).
* The API key is held only inside function values the client holds, is sent only in the JSON body of
  requests to `api.neverbounce.com` or a loopback test server, and never appears in a URL, an
  error string or formatted output. A way to make the SDK print or send the key elsewhere is a
  vulnerability: report it privately as described above.
* Error strings never contain email addresses, row data, URLs, callback headers, response bodies or
  provider messages. Provider messages are kept on errors only when the application opts in with
  `WithProviderDiagnostics`, and the key is scrubbed from them.
* The library never logs, prints, stores or caches data, and never reads credentials or
  configuration from the environment (the default transport honors the standard proxy variables).
  Verification results are contact data: access control, logging, retention and lawful use are
  the calling application's responsibility.

<br/>

## 🛡️ Security Standards

We follow the [OpenSSF](https://openssf.org) best practices to ensure this repository remains compliant with industry‑standard open source security guidelines.

<br/>

## 🛠️ GitHub Security Workflows

To proactively protect this repository, we use several automated GitHub workflows:

- **[CodeQL Analysis](./.github/workflows/codeql-analysis.yml)**: Scans the codebase for security vulnerabilities and coding errors using GitHub's CodeQL engine on every push and pull request to the `main/master` branch.
- **[OpenSSF Scorecard](./.github/workflows/scorecard.yml)**: Periodically evaluates the repository against OpenSSF Scorecard checks, providing insights and recommendations for improving supply chain security and best practices.

These workflows help us identify, remediate, and prevent security issues as early as possible in the development lifecycle. For more details, see the workflow files in the [`.github/workflows/`](.github/workflows) directory.
