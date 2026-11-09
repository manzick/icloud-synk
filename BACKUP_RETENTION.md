# Replacement backup retention

Set `backup_retention` in the existing JSON configuration:

```json
{
  "backup_retention": 3
}
```

This is a configuration fragment, not a complete configuration.

| Value | Behavior |
| --- | --- |
| `-1` | Unlimited retention, the default for new and existing configurations. |
| `0` | Move eligible recovery backups to iCloud trash after successful verification. |
| positive N | Keep the newest N eligible backups per original path and remote parent. |

Values below `-1`, fractional values, and strings are unsupported. Omitted values
retain the existing unlimited behavior. Retention does not permanently purge
iCloud trash. It operates independently of `delete_remote` and `protect_deletions`:
those continue to govern normal user-file deletion propagation; explicit finite
retention authorizes cleanup only of proven completed replacement backups.
Conflict copies remain ordinary synchronized files and are never collected.

## Ownership and compatibility

A filename is never sufficient proof of ownership. Replacement operations record
an additive `replacement_artifacts` journal in `.icloud-synk/state.json`. Records
bind the original relative path, remote parent ID, artifact kind, exact Drive and
document IDs, zone, filename, size, ETag and modification time. A persisted sequence
orders managed backups without depending on filename timestamps or clock changes.

The verified temporary file is recorded before moving the old version. The
confirmed old-file rename records its backup identity. Only after final remote
verification, the local-source checks, and saving the new baseline does that
backup become eligible. Retention never updates unrelated reconciliation entries.

Historical backups, including backups created by older versions, have no journal
proof and are neither adopted nor deleted. The effective total can therefore
exceed N. Losing the journal loses cleanup authority; it never grants authority
based on a matching name. Renamed, edited, moved, or identity-mismatched artifacts
are preserved. Do not manually edit provenance to force cleanup.

The enclosing state version remains 1: existing state loads without migration.
Older binaries may discard this optional field on saving state, leaving subsequent
versions unable to prove ownership. Configuration loading supplies `-1` when the
new option is absent. Avoid sharing one state file across concurrent processes or
different accounts.

## Failures, interruption, and dry-run

Every replacement still stages the new version, verifies it, retains the old
version during the swap, verifies the final target, and uses the existing guarded
rollback on failure. Pending temporary, recovery, and failed artifacts are never
automatically deleted. A pending record blocks retention for its original path;
resolve interrupted operations manually after inspecting all versions. Crashes
before provenance can be saved can leave unrecognized artifacts, which are also
preserved. No automatic adoption, rollback recovery, or temporary-file cleanup is
introduced.

After transfers, finite retention runs in deterministic path order, keeps the
newest journal sequences, and trashes the oldest eligible versions first. Before each trash request it re-lists the parent, checks the exact
backup identity and metadata, requires the original to match the verified state,
and downloads the original again to verify its saved SHA-256. Trash requests use
the freshly checked ETag. A subsequent listing must confirm the artifact is gone
before its provenance record is removed. Missing artifacts can be forgotten after
listing their known parent, making lost-response retries idempotent.

Cleanup failures are warnings and increment `cleanup_errors` in sync statistics;
they do not invalidate a successful upload or roll back its correct baseline.
Remaining backups are retained, and the next real pass retries cleanup, including
passes with no transfers. Dry-run does not invoke retention, trash requests, or
state writes. It does not predict a cleanup count because eligibility requires
live identity and content verification.

The cloud API does not provide a transaction spanning target verification and
backup trashing. An independent actor can still change or remove the target
between requests. ETag checks protect the trash candidate against concurrent
changes; do not treat retention as a substitute for independent backups. Finite
retention adds verification downloads and metadata requests, favoring data safety
over throughput.

## Drive verification

`icloud-synk verify-drive` still inventories the selected Drive folder and hashes
normal local/remote file contents. It reads the artifact journal from the same
target directory but never modifies Drive files or synchronization state. Missing
state grants no ownership; corrupt or incompatible state produces an error.

Remote-only artifacts whose exact live identities and metadata match the journal
are excluded from comparison counts. In addition, remote-only regular files with
the generated `backup` filename format are excluded when provenance is absent,
using the same replacement-name classifier as synchronization. Each is explicitly
listed under `Skipped internal artifacts` and `INTERNAL ARTIFACT SKIPPED`; filename-only
classifications say `provenance unavailable, ownership not established`. This
covers retained backups created before the current journal existed. It does not
import provenance, prove ownership, or authorize retention cleanup.

Known unfinished temporary, recovery, or failed artifacts remain `UNVERIFIABLE`.
Changed or inconsistent journal records cannot be bypassed by filename recognition,
including an identity change at a recorded artifact path. Unknown `new`/`failed`
artifacts and malformed backup-like names undergo normal comparison. Directories
and packages are never excluded by filename alone. A local counterpart is always
compared as a normal file; a pending artifact also keeps verification incomplete.

The generated backup namespace is reserved by synchronization. A user-created
remote-only file with exactly that naming format cannot be distinguished from a
historical backup without provenance. Verification explicitly reports this
classification instead of silently omitting it. Rename such user files outside
that namespace if they should participate in normal verification.

Counter semantics (after configured exclusions):

* `Local files`: files in the local comparison inventory, including local artifact counterparts.
* `Files in iCloud`: files in the remote comparison inventory **after** internal-artifact
  filtering, not the raw inventory total.
* `Identical files`: files whose actual local and downloaded remote contents matched.
* `Skipped internal artifacts`: remote-only artifacts excluded from comparison,
  including explicitly labeled filename-only backups. Unfinished proven artifacts
  may appear here but still prevent success through `UNVERIFIABLE`.

Exit status is 0 only when all comparison files match and there are no differences,
unverifiable entries, or skipped local entries. Skipped completed or historical
backups alone do not prevent success. Verification never downloads excluded
backups, changes remote data, or writes synchronization state.
