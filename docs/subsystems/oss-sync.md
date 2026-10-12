# OSS backup commits

OSS is a configuration entry of the S3 backend. A recognized regional
`s3.oss-<region>.aliyuncs.com` endpoint, including `-internal`, identifies it.
The [S3 transport](../../internal/davsync/s3.go) signs ordinary snapshots,
usage and request archives with AWS SigV4. Only the backup object's `get` and
`put` use the [journal protocol](../../internal/davsync/oss_backup.go).
Archives retain raw S3 behavior even when their bucket is OSS.

## Commit and read

1. Read the journal's bounded 4 KiB suffix with `Accept-Encoding: identity`.
   Validate the byte range, complete JSONL records, schema and references.
   Every record is at most 1 KiB and contains only an opaque snapshot name
   and the encrypted payload's SHA-256, without configuration contents.
2. A read gets the last record's encrypted snapshot and checks its SHA-256.
   A logical version token holds the committed byte length and record hash;
   this token is never sent as an S3 `If-Match` header. A reader with the
   same token may reuse its previously validated immutable snapshot cache.
3. A writer checks its expected version, uploads a fresh random snapshot
   under `magpie/snapshots/`, and appends one record at the expected journal
   byte length. AppendObject's position check is the sole commit point.
   Competing writers at that position cannot both commit. HTTP 409
   `PositionNotEqualToLength` becomes `errChanged`; `syncNow` re-reads and
   runs the whole existing merge again before retrying.

The journal is `magpie.magpie-backup.journal`, beside the legacy backup.
Native Append uses [OSS4 signing](../../internal/davsync/oss.go), a derived
same-region OSS endpoint, signed Content-MD5 and strict next-position
verification. Native requests do not follow redirects. There is no lock,
lease expiry, owner deletion or fencing claim. Snapshot uploads alone do
not commit a backup. This does not depend on `x-oss-forbid-overwrite`, which
can lose its protection on versioned buckets.

## Migration and failures

Before the first journal commit, `get` can read the legacy single file.
The first writer checks its legacy ETag and payload hash before Append at
position zero. The legacy file is preserved, and no compatibility mirror
is written. **All devices that write this OSS sync target must upgrade.**
An older client writes only the legacy file and cannot participate in the
journal protocol; its later edits are not journal commits.

Missing, damaged, oversized or incomplete journal tails and invalid or
missing referenced snapshots are errors, never an empty backup. A client
that previously saw a journal cannot fall back to legacy when it vanishes.
Restore, Upload and a missing local cache retain the known journal version
only when the stored state matches the configured target. Their forced
reads still fetch and validate the snapshot even with an unchanged token;
state from another target cannot block legacy migration.
Fresh reads validate the snapshot; cached reads rely on the protocol's
immutability, so manual object replacement or removal is unsupported.

A known position conflict permits deletion of only that writer's fresh
uncommitted snapshot. A crash before commit can leave an orphan. A lost
Append response, HTTP 5xx or unexpected next-position leaves the snapshot
in place because the commit might already exist; the next read resolves
the visible state. There is no automatic snapshot garbage collection.
Do not remove journal records or snapshots referenced by them to recover
an unknown result. A corrupt journal needs deliberate recovery from a
known valid encrypted backup, without pretending the old version is current.

## Configuration and verification

Only a brand-new OSS form defaults its prefix to `backups`, making the
backup directory `backups/magpie/`. Existing empty or custom prefixes stay
as saved. The form shows the resulting directory as the prefix changes;
changing this field selects another target and does not move remote data.

```sh
go test -tags nogui ./internal/davsync -run 'TestOSS' -count=20
node --test internal/gui/tests/s3-sync.test.cjs internal/gui/tests/gui-zh-tw.test.cjs internal/gui/tests/gui-ja.test.cjs internal/gui/tests/gui-de.test.cjs
```

Tests cover an independent official SDK OSS4 signature vector, competing
different backups and re-read merge, full `syncNow` conflict retry, legacy
migration, response loss, missing next-position, redirects, suffix reads of
logs over 64 KiB, invalid ranges, malformed records and snapshot corruption.
They also cover multi-segment regional endpoints (including their internal
forms), forced-read journal disappearance and target-scoped state hints.

Live checks use a separate opt-in test executable, a sandbox home, synthetic
data and a unique authorized child prefix. Credentials are supplied through
private stdin, and cleanup verifies only objects attempted by that run.
The regional Shenzhen endpoint was checked; other regions, internal and
accelerated endpoints, and version-enabled buckets were not live tested.
Append's position behavior and OSS object consistency follow Alibaba's
[AppendObject contract](https://help.aliyun.com/zh/oss/developer-reference/appendobject)
and [OSS documentation](https://help.aliyun.com/zh/oss/user-guide/).
