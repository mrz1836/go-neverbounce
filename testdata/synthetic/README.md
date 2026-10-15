# Synthetic fixtures

SDK-authored fixtures for the NeverBounce API v4.2 response shapes.

Every file here was written from scratch to the shape the NeverBounce API v4.2 reference documents.
Nothing is copied from the provider's documentation, and no real responses or contact data are
committed. Addresses use the reserved `example.com`, `example.org` and `example.net` domains, and
every name, ID and counter is made up.

Files are grouped by operation prefix:

| Prefix | Operation |
|---|---|
| `check_` | `single/check` |
| `create_` | `jobs/create` |
| `status_` | `jobs/status` |
| `results_` | `jobs/results` |
| `envelope_` | failure envelopes shared by every operation |

CSV download bodies are never stored here: `.gitattributes` and `.editorconfig` would rewrite their
line endings, so tests build them as Go string literals.
