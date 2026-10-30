# Investigation: reconciliation after file replacement

Scope: source at c5de2fe on `fix/sync-state-after-upload`, plus deterministic
tests only. No account, configuration, session, or real sync state was read.
No live synchronization or authentication was performed. Production code is
unchanged.

## Conclusion and limits

The strongest code-supported hypothesis is an unreliable remote baseline after
upload: confirmed replacement metadata is discarded, then a name-based refresh
can install older metadata or leave the ETag unknown. A later ETag change or
remote size mismatch also schedules a download. These mechanisms reproduce
offline, but none establishes which occurred in the reported Apple Account.

The reported nonempty ETag `b6::b5` specifically contradicts an *empty-ETag-only*
explanation of the current download. Given the verified local size and mtime,
`localChanged` is false. Under this source, a planned download therefore requires
a different remote ETag or size (assuming the same path and state file are used).
Remote identifiers and timestamps alone cannot produce that download. The
remote ETag/size comparison is still the missing evidence. The string's internal
format is treated as opaque; it does not establish content identity or freshness.

The earlier conflict note establishes that BOTH predicates were true before
the replacement. A defect after that replacement cannot, by itself, explain
that earlier classification. A baseline problem after the initial creation is
one possible link; genuine remote modification or missing state are alternatives.

## Lifecycle in the current source

1. `Scanner.Scan` inventories local size/mtime without hashing. `fetchRemote`
   lists folders breadth first, resolves `drive_subfolder`, and registers its
   root at `""` (engine.go:181–260). Missing requested folders abort discovery.
2. `Reconcile` takes a state snapshot. For files present on both sides,
   local size/second-truncated mtime and remote ETag/size determine the action
   (reconcile.go:150–160, 207–233). SHA-256 is not used in planning.
3. Both predicates true invokes `conflictOp`; with `both`, local mtime strictly
   after `Item.ModTime()` selects upload; ties select download. Remote time
   prefers DateModified, then DateChanged, then DateCreated.
4. `upload` first hashes same-sized local content against the saved hash to
   suppress timestamp-only uploads (engine.go:694–701). This happens after
   planning and is absent from dry-run. For a local-winning `both` conflict it
   downloads the old remote version into a local conflict filename (704–710).
   That file is not recorded in state and is an ordinary new local file next
   pass, so uploading it is current policy behavior.
5. For an existing remote path, upload re-lists the parent, uploads a temporary
   sibling, confirms its name, downloads and verifies its hash, renames the old
   file to backup, renames the temporary file to the original name, confirms its
   identity, and downloads/verifies its hash again (732–883). Failures retain
   versions and may attempt guarded rollback. The refreshed old item is selected
   by name; no comparison against the planned baseline reclassifies a concurrent
   edit here. Preservation is therefore especially important.
6. A remote backup is retained for EVERY replacement, including a normal
   non-conflicting local edit. This is independent of `conflict: both` and
   deletion settings. Internal replacement siblings are skipped by reconciliation.
7. Despite having the final `target` metadata, upload records the local size,
   pre-upload stat mtime, uploaded hash, new IDs, and an EMPTY ETag (886–904).
   It marks the parent stale. Initial creations also take this path, but lack a
   confirmed remote target and may initially retain empty/previous IDs.
8. `apply` calls `refreshEtags` after transfers (421–424). Refresh updates ETags,
   IDs, zone and remote time by sanitized path for ALL tracked files in each
   stale parent, preserving local size/mtime/hash (1250–1300). It neither checks
   expected identity/size nor verifies content. It silently accepts omitted
   folders/items, and clears the stale set before attempting the request.
   Errors are debug-logged without incrementing transfer errors. Refresh saves;
   `SyncDrive` also saves afterward and only warns on save failure (172–176).
9. Next pass repeats discovery. Empty/different ETag or different remote size
   schedules download when local facts match. Matching metadata yields `record`,
   which is normal bookkeeping, not a transfer. Repeated dry-runs cannot repair
   the baseline, so overnight persistence does not prove an ongoing server delay.
   A real non-conflict download can detect equal hashes and preserve the local
   inode/mtime, but still transfers remote bytes first (565–588).

## State sufficiency and safety

`Entry` (state.go:19–35) can represent a completed local/remote baseline with
IDs, opaque ETag, size, local hash and both mtimes. It cannot represent why the
ETag is empty, pending creation/replacement, failed refresh, or a verified
replacement later overwritten by a stale refresh. One shared size assumes
convergence; no independent remote size or remote content hash is stored.

IDs are saved but ignored by `remoteChanged`. Same-size replacement with the
same ETag but a new ID is missed. Remote timestamps select a conflict winner,
not whether the remote changed. A metadata-only ETag change counts as a remote
change. Equal local size and mtime can miss local content edits; the saved hash
does not alter that predicate. Consequently, the format and current logic cannot
reliably distinguish all four requested categories: legitimate remote edit,
own successful upload, incomplete refresh, and two actual content edits.

Refresh has an additional safety issue: uploading one file may accept a remote
edit to an unrelated tracked sibling after discovery, without downloading it.
Its changed ETag can become the baseline, masking that change on a future pass
when sizes match. Refresh should be scoped to pending uploads, not all siblings.

Other possibilities remain: true remote modification; ETag evolution after a
successful refresh; remote size inconsistency; wrong invocation/state path;
save failure leaving an older baseline; or a local edit during upload (stat,
read, and hash do not form an immutable snapshot). None was observed live here.
The verified current local metadata contradicts a current local-change cause.
The subfolder root is correctly registered in source; no evidence supports it
as the leading explanation. HTTP/2 and deployment are outside this investigation.

## Tests added

`internal/synk/engine_upload_state_test.go` uses an in-memory drive client and
temporary files/state. No network calls or credentials are involved.

- Stable lifecycle: synchronized file, local edit, non-conflicting upload,
  two verified replacement downloads, two renames, retained remote backup,
  successful refresh, saved state reload, then record only. Applying that next
  plan causes no transfer, rename, conflict copy or extra backup.
- Characterization cases: refresh error, missing folder, missing item, stale
  old item, later ETag change, and size-only change each produce a subsequent
  non-conflict download with unchanged local size/mtime/hash. Stale refresh
  overwrites even the confirmed new identity.
- Classification evidence: ETag/size drift, empty ETag and missing state turn
  a local edit into a conflict. Identity-only and remote-time-only changes are
  ignored by change detection.

The defect cases deliberately assert CURRENT behavior so this investigation
does not leave the suite failing. They must be revised alongside an approved
fix. The stable requested scenario already passes; it is not guaranteed when
refresh is incomplete or inconsistent. Later ETag/size changes may be genuine
edits and must not simply be suppressed.

## Minimal proposed fix, not implemented

Persist the exact metadata from the already verified replacement target together
with the uploaded local facts. Do not clear a known ETag or let an unvalidated
folder refresh overwrite that confirmed baseline. Restrict refresh to uploads
that still need metadata (especially initial creations), validate requested
folders and expected identity/size, and surface incomplete refresh explicitly.
Use content verification when metadata ambiguity cannot safely establish
convergence; do not blindly accept all post-upload ETags. Persisting a pending
upload marker may be needed for reliable recovery of unconfirmed creations
across restarts; that is broader than the replacement-only fix and needs a
separate schema/compatibility decision.

Existing valid v1 completed entries can support the replacement-only change
without migration. Older empty or stale entries cannot safely be inferred to
be successful uploads. Genuine remote changes must remain visible. Keep backup
retention, conflict preservation and deletion protection intact. Existing conflict
copies remain ordinary files; a production fix should not remove or hide them.

After a fix, stable convergence must still pass; confirmed replacements must
also converge across refresh errors/omissions/stale identity and restart without
losing confirmed metadata. Tests must additionally cover concurrent genuine
remote edits, same-size identity changes, unrelated sibling changes, pending
creation recovery, and replacement/verification failure retention. A later
genuine content change must still download or conflict as appropriate. An ETag
change alone cannot universally guarantee zero network reads without stronger
server evidence.

## Implementation follow-up

The approved fix retains verified replacement metadata directly and removes
the folder-wide `refreshEtags` mechanism. Initial creations now confirm their
exact name, complete file metadata, size and SHA-256 before advancing only that
file's state. Missing/inconsistent metadata and verification failures are
reported as transfer errors and leave the prior state intact. No pending-upload
schema or state migration was introduced. After an interrupted or unconfirmed
creation, recovery deliberately uses normal conservative reconciliation, which
may download or preserve a conflict rather than infer upload ownership.

Uploads use a private temporary snapshot, including retries, and recheck local
identity, size, mtime and content before committing the baseline. This adds
temporary disk use proportional to the file size; initial uploads additionally
require a verification download. Known remote identity changes now trigger
reconciliation even when ETag and size match. A replacement whose remote
metadata changed since planning aborts before upload/rename. Existing backup,
conflict and guarded rollback behavior remains in place.

The characterization tests now assert convergence after confirmed replacement,
conservative failure for incomplete creations, persistence across reload,
genuine remote edits and identity changes, sibling isolation, source mutation,
and retention/restoration on failed verification. The investigation above is
retained as the historical description of the pre-fix implementation.
