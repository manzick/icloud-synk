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

Only remote-only artifacts whose exact live identities and metadata match the
journal are excluded from normal file counts. They appear under `Skipped internal
artifacts` and `INTERNAL ARTIFACT SKIPPED` in the English report. Completed backup
artifacts do not prevent success. Known unfinished temporary, recovery, or failed
artifacts are reported separately and also marked `UNVERIFIABLE`, so an interrupted
replacement cannot quietly make the report successful. A local counterpart is
still compared as a normal file; a pending artifact also keeps verification
incomplete in that case.

Exit status is 0 when all normal files match, including when only proven completed
backups differ. It is 1 for differences, unverifiable files, unfinished artifacts,
or operational errors. Historical artifacts with no provenance, changed artifacts,
and user files with backup-like names are not silently ignored: they undergo
normal comparison and can cause a difference. Unknown temporary artifacts are
likewise ordinary comparisons. There is no automatic historical-backup import or
name-based exclusion switch; review historical files manually if they need to be
retained elsewhere or removed.
