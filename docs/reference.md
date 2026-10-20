# Behavior and protocol reference

See the [README](../README.md) for an introduction and setup instructions.

## Agent workflow and contract

Every tool returns a typed structured envelope `{ "data": ... }` on success or
`{ "error": { "code": "...", "message": "...", "details": ... } }` with MCP
`isError: true` on domain errors. Input/output JSON schemas are SDK-generated.
Tool annotations advertise read-only, idempotent, closed-world behavior; the code
enforces it independently. All source text is untrusted data, not instructions.

| Tool | Inputs | Result |
| --- | --- | --- |
| `workspace_list` | optional `cursor`, `limit` | Configured metadata and capabilities |
| `workspace_info` | `workspace_id` | Capabilities, health, provenance, summary |
| `workspace_status` | `workspace_id` | Same observation view as info |
| `workspace_snapshot` | `workspace_id` | Latest persisted revision metadata; never captures |
| `workspace_files` | `workspace_id`, optional `revision`, `prefix`, `cursor`, `limit` | Sorted manifest page and resolved revision |
| `workspace_read` | `workspace_id`, `revision`, `path`, optional `start_line`, `end_line` | UTF-8 text, digest, range and truncation metadata |
| `workspace_diff` | `workspace_id`, `from_revision`, `to_revision`, optional `path`, `cursor`, `limit` | Sorted changes and bounded unified hunks |
| `workspace_changes` | `workspace_id`, optional `after_cursor`, `limit` | Ordered events, next cursor and earliest/latest positions |

For example, call `workspace_snapshot({"workspace_id":"my-project"})` and
retain its `data.revision` as A. After editing, get B the same way, then call:

```json
{
  "workspace_id": "my-project",
  "from_revision": "<A>",
  "to_revision": "<B>"
}
```

with `workspace_diff`. `workspace_read` with A still returns the original bytes
after files are changed or deleted. Revisions are random `rev_` identifiers;
lexicographic ordering has no meaning. `evt_` cursors are per-workspace event
positions, not Git commits. Keep each cursor with its workspace ID.

Pages default to 50 entries, maximum 200. Manifest/diff cursors are tied to their
workspace, revision(s), filter, and effective capture policy. Pass the resolved `revision` with subsequent
file pages; a cursor without a revision is rejected. Listing without a cursor is
an explicit resynchronization to retained history. With an expired event cursor,
`CURSOR_EXPIRED` includes earliest/latest positions. An exact immediately preceding
position is valid because no events after that position have been lost.

File lines are 1-based, inclusive, and preserve LF/CRLF and final-newline state.
Reads and diff pages budget 128 KiB of encoded content, plus envelope metadata;
the SDK supplies both structured and JSON text forms, so HTTP wire size is larger.
`next_line` resumes truncated reads. A single line larger than the response budget
returns a UTF-8-safe prefix with `partial_line: true`; line paging cannot retrieve
the remainder of that single line in v1. Binary (invalid UTF-8 or NUL-containing)
files are stored but return `BINARY_CONTENT` on text reads.

Diffs use deterministic whole-file replacement unified hunks, preserving line
endings and marking missing final newlines. They do not run Git or an external
program. Renames are deletion plus addition. Each diff hunk is capped at 16 KiB;
`change.truncated` explicitly means it is incomplete. Page cursors advance by file;
use historical line reads to inspect text omitted from a large diff. Omitted and
binary content yield metadata instead of a fabricated textual diff. File mode
changes count as modifications.

See [OpenClaw configuration](../examples/openclaw.md). OpenClaw, an editor, and an LLM runtime are not dependencies.

## Observation and security model

The provider-independent domain is in `internal/workspace`; Git capture,
SQLite/blob storage, scheduling, and MCP are separate packages. No shell, Git
mutation, test execution, client-chosen root, or capture command is exposed by MCP.

The Git adapter uses NUL-delimited `ls-files --stage`, eligible `ls-files --others`,
and metadata-only `rev-parse`/`symbolic-ref` for HEAD and branch. It captures actual saved working-tree
bytes for staged, unstaged and untracked files. Index metadata is a digest of
eligible stage entries, **not a parallel index-content tree**. Staged deletion
with remaining working-tree bytes is included. Missing files are absent. Git
submodules are opaque omitted gitlinks. Empty repositories and detached HEAD work.
`git_history` capability is false: workspace history is supported, arbitrary Git
commit history is not.

Git runs with fixed argument arrays, no pager, optional locks disabled, FSMonitor
and untracked caches disabled, hooks disabled, and a minimal environment that
removes inherited Git/config/loader overrides. Lazy fetching and all Git transport
protocols are explicitly disabled. No Git remote operations or
repository programs run. Git output and file reads are bounded. Repository Git
config is still parsed by Git; the installed Git executable itself is trusted.

The regression test `TestNeverRunCleanFilter` verifies that `git status` executes configured
clean filters while comparing working-tree content, despite optional locks and
FSMonitor being disabled. The observer therefore never invokes `status` or `diff`.
NUL-delimited enumeration plus rooted byte reads provide the same source coverage
without entering Git's content-filter pipeline. Tests use porcelain status only
on trusted fixtures to verify observation caused no changes.

Rooted `openat` directory-descriptor traversal uses `O_NOFOLLOW` at **every** path
component, stronger than `os.OpenRoot`'s permitted internal symlinks for this
no-symlink policy. Final opens use `O_NONBLOCK`, and fstat rejects nonregular files
before any read (including FIFO/device/socket replacements). File identity, size,
mtime and mode are rechecked, along with the eligible Git/path state. Detected
instability retries three times, then reports `CAPTURE_UNSTABLE` without publishing.
No successful capture claims OS-level multi-file atomicity: undetectable cross-file
race windows and malicious timestamp-preserving rewrites remain possible.

Manifest changes (including executable/omission metadata), eligible index digest,
HEAD and branch changes publish revisions. Identical recaptures update health
without duplicate events. Availability failures/recovery are recorded in durable
health, not separate events. Health contains attempt/success timestamps, latest
revision, safe error code and consecutive failures. Failed captures never replace
the last complete snapshot. Startup capture is asynchronous; an empty store can
briefly return `REVISION_NOT_FOUND`.

Built-in component exclusions, applied even to tracked files:
`.git`, `.env`, `.env.*`, `*.pem`, `*.key`, `id_rsa*`, `id_ed25519*`, `*.p12`, `*.pfx`,
`node_modules`, `.venv`, `venv`, `dist`, `build`, `target`, `.cache`.
Denied paths never enter manifests or client errors. Administrator `exclude` globs
are additive, and optional `allow` globs restrict further. Custom globs use
case-sensitive doublestar matching against the **entire slash-separated relative
path**: `*` matches within one component, `**` crosses directories, and
`**/credentials/**` includes all descendants. An allowlist cannot override a deny.
Git-ignored untracked files are excluded; tracked ignored files remain eligible.
Invalid UTF-8 filenames, absolute/traversal paths, percent-encoded paths, NUL,
backslashes and colon-containing paths are excluded/rejected rather than repaired.
Spaces, tabs, newlines and Unicode filenames are supported.

Limits default to 5,000 eligible paths, 1 MiB per file, 50 MiB captured bytes.
Oversize files are explicit omitted entries. Excess total/count limits fail the
whole capture. Built-in excludes are not a complete secret scanner: review roots
before authorizing them, and extend exclusions for credentials specific to your
project. Never allowlist a repository merely because these patterns exist.

HTTP accepts literal loopback IPs only. Bearer tokens are required even locally;
Origin, when present, must exactly match the endpoint's HTTP origin. Native clients
may omit Origin. Host must match the listening address, so use `127.0.0.1`, not
`localhost`, with the default configuration. SDK localhost protection remains
active. Request bodies are capped at 1 MiB; headers and read/write timeouts are
bounded. Logs contain workspace IDs/error codes, never source bodies or tokens.
The daemon is single-administrator, with one token granting access to all configured
workspaces. Same-user filesystem attackers and hostile administrators are outside
this boundary. Store and token locations must be administrator-controlled.

## Persistence and retention

State lives outside every observed root (canonical paths are checked). New
directories are 0700 and files 0600. SQLite uses WAL, foreign keys, full synchronous
commits and a versioned schema. Manifests are normalized immutable JSON in revision
rows instead of a separate `revision_files` table; events are separate durable
rows. SHA-256 blobs use two-level paths. Blob bytes and directories are synced
before a transaction publishes the revision/event together. The latest revision is
derived from committed rows; a crash cannot advance it independently. Orphaned
blobs from failed transactions are safe and collected later. An OS advisory lock
prevents competing daemon/CLI writers and releases automatically on process exit.

Retention runs at startup, hourly, and after manual captures. It removes oldest
unpinned revisions older than seven days, then oldest unpinned revisions until
unique referenced **blob bytes** fit `storage.max_bytes`. Every workspace's newest
revision and corresponding event are pinned.
Retention always preserves a contiguous event suffix per workspace, including
when the system clock moves backwards; this can retain extra old revisions.
The budget is deliberately soft:
pins may exceed it; SQLite metadata, tombstones, WAL, filesystem allocation and
short-lived orphan blobs are not included. It is not a hard disk quota. Pruning is
transactional; GC only removes blobs unreferenced by remaining manifests. Expired
revision tombstones distinguish `REVISION_EXPIRED` from `REVISION_NOT_FOUND`.
Expired event prefixes return `CURSOR_EXPIRED`, never silently skip lost events.
Tombstones and SQLite allocation can grow; `forget` removes a workspace's logical
history but does not guarantee secure disk erasure or shrink SQLite's file size.

Snapshots retain deleted source lines until retention/GC. Disk encryption/backups
are an administrator responsibility; the bridge provides permissions, not at-rest
encryption or secure erasure. Tightening exclusions filters even historical MCP
lists/reads/diffs immediately, but previously stored bytes remain on disk until
retention or `forget`.

The compact schema, soft blob budget, whole-file diffs, line-based long-line limit,
health-only availability transitions, avoidance of Git porcelain content checks,
and exclusive-lock local CLI are design tradeoffs. There is no stdio frontend, subscription/resource API, arbitrary Git
history API, filesystem watcher, manual capture over MCP, test runner, or outbound
agent notification.

