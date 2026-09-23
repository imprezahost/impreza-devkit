# Advisory release tooling

Candidate tooling; no public feed or signing trust is implied by this source.
The agent fetches data, never executes this publisher-side tool. Build with the
repository's pinned Go toolchain: `go build -o advisory ./tools/advisory`.

## Trust boundaries

Collection has no signing key. It reads only the fixed OSV npm, Packagist and Go
exports, retaining GitHub-reviewed and official Go records. A separate signer
receives the reviewed candidate, pinned public key and previous signed release.
Keep the seed in a private regular file (0600 on Linux), outside the collector,
repository, webroot, command line and environment. Work in directories writable
only by the operator. Production operation targets Linux; Windows is for local
development and requires the operator to restrict ACLs.

The signing seed is a base64-encoded Ed25519 seed; the public file contains its
base64-encoded public key. Key enrollment/backup is an operator ceremony, not a
download or automatic trust-on-first-use operation. Test keys must never be used
for production. Root or signing-authority compromise is outside this boundary.

## Collect, review, sign, verify

Use unique paths for each run. Every output refuses replacement. Keep the OSV
archive hashes/counts/partial-record IDs from stdout with the review evidence.

```sh
./advisory -mode collect -revision 2026092301 -out candidate.json > collection.jsonl
./advisory -mode review -input candidate.json -previous previous.signed.json \
  -public-key-file trusted-public-key.txt -out review.json
sha256sum review.json
```

Review the **contents**, not just the checksum. The report binds exact candidate
and predecessor bytes and the public key. It compares record/entry coverage for
each of the four ecosystem/source pairs and counts additions, removals and
changed records. It warns on more than 5% loss/removal, more than 25% growth or
more than 25% changes in each source. Thresholds are investigation triggers,
not proof of authentic or complete data. Check upstream anomalies, withdrawals
and the collection report's partial IDs. Unsupported data stays explicitly
incomplete; no guessed corrections are made.

The first release requires `-bootstrap` **instead of** `-previous`; use it only
for initial enrollment. Later releases must advance the revision and must not
move generation time backwards. An expired predecessor can be authenticated to
recover a delayed feed, but the new candidate must be unexpired. Keep revisions
monotonic independently of wall-clock corrections.

Copy the approval SHA256 through the operator's trusted review channel. On the
isolated signer, use the exact reviewed files:

```sh
./advisory -mode sign -input candidate.json -previous previous.signed.json \
  -public-key-file trusted-public-key.txt -review review.json \
  -review-sha256 APPROVED_REVIEW_SHA256 -seed-file /private/signing/seed \
  -out candidate.signed.json
./advisory -mode verify -input candidate.signed.json \
  -public-key-file trusted-public-key.txt -out verified.json
```

Signing recomputes the review, checks the supplied approval hash and requires the
seed to match the reviewed public key. Changing any input invalidates approval.
When applicable, signing additionally requires `-allow-incomplete` and/or
`-allow-coverage-change` after explicit review. They do not bypass missing sources,
invalid signatures, expiry, rollback, mismatched review hashes or changed inputs.
For initial enrollment, pass the same `-bootstrap` choice used during review.

`verify` authenticates the envelope against the independent key, checks expiry
and required source coverage, and emits the artifact hash/revision. It does not
compare against a live server and cannot prove that a candidate is newer than a
different release that another operator already published.

## Distribution and scheduling

These commands do **not** publish, run a scheduler or mutate a release ledger.
Before activation, the distribution transaction must lock the shared release
state, compare the live predecessor hash with the reviewed predecessor, verify
the artifact, replace the feed atomically and retain a durable receipt/backup.
Do not use bootstrap to bypass a missing/corrupt ledger. Recovery must preserve
the revision floor; clients correctly reject a rollback to an older database.
Embed the independent public key in the agent release, then verify a clean
installation and refresh from the public download.

A daily collector should alert on failure, missing sources, coverage changes and
approaching expiry. A success log alone does not mean publication succeeded.
Signature validity is at most seven days; agents refresh every six hours and
retain the last verified snapshot as stale after expiry. Keep collection and
signing identities/storage separate; do not pipe unreviewed collector output
directly into a signer or silently approve warnings in a scheduled job.

## Explicit key generation

`advisory -mode keygen -out NEW_PRIVATE_DIRECTORY` creates an Ed25519 seed,
public key and SHA256 fingerprint with owner-only permissions. The directory
must not exist. Repeated or incomplete enrollment requires investigation, never
a silent replacement. Store the seed outside collection and distribution paths.
`verify -allow-expired` authenticates an old ledger artifact for recovery; it
does not extend its lifetime or make it a valid fresh update.
