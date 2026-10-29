# Recovered production source baseline

This independent repository preserves the recovered source candidate for the
New API custom image running on the production server on 2026-10-29.

- Upstream base: `f2c7cd33c853fbbd2dc8f1701123888c98ffe00e`.
- Recovery: ten original patches recorded before the 2026-07-11 image build,
  restoring five files for the custom asynchronous image API.
- Running OCI image: `sha256:f1e75962cbb17ce479f9bdccceecba480810411cf4839358282d4ad801a3d701`.
- Running binary SHA256: `e47f54b446133e74158f570434f288d62a2701b911a4510d724cd381cf4136c8`.
- Matching image archive, private deployment configuration, patch evidence and
  checksum manifests are stored in the parent recovery directory.

The server and runtime image do not contain the Go source. This tree was
recovered from the original upstream commit and historical source patches;
it was not decompiled. A bit-for-bit reproducible rebuild has not been proved.
The downloaded original image is the verified production rollback artifact.

This baseline excludes later image-result storage changes and H3 integration
changes. No production service or database was changed during recovery.

Only the official `upstream` fetch remote is configured. Set a private publishing
remote separately when a destination has been chosen. Develop future changes
on feature branches and record the source commit and image ID for each release.

## Local maintenance branch (2026-10-29)

`feat/h3-latest-main` starts from official main
`e449cc700eb569cdfc2199cb73e97de46944f43c` (latest release observed:
`v1.0.0-rc.47`). The original `production-2026-07-11` recovery branch is retained.
Custom async image endpoints were adapted to the new shared protocol router;
the original five-file patch cannot be applied unchanged to this version.

H3 is native channel 64 (`H3 Studio`), model `h3`. Configure the Studio HTTP(S)
base URL and operator key in the channel. Runtime requirements for new H3
reservations: no `REDIS_CONN_STRING`, `BATCH_UPDATE_ENABLED=false`, wallet billing.
Redis/batch configurations fail closed before charging or upstream submission.
Changing those settings while a task is pending can defer its refund.

The fixed contract is USD platform credit 0.09 per requested video second,
4-15 integer seconds. Five seconds cost 0.45 (225000 quota at 500000 quota/USD).
Generic administrator price overrides and text/group discounts do not change
this native H3 contract. Existing recharge and text/image pricing are unchanged.

POST `/v1/videos` or `/v1/video/generations` with JSON:

```json
{"model":"h3","prompt":"example","seconds":"5","size":"1344x768"}
```

Send an `Idempotency-Key` to safely replay the same submission. Fetch
`/v1/videos/:task_id`; upload media directly to its scoped GPU target and submit
only receipts to `/v1/videos/:video_id/inputs`. The transaction stores the task,
unique reservation, frozen request, and wallet/token debit before networking.
Uncertain transport responses remain pending for reconciliation; only confirmed
provider failure permits an atomic, exactly-once refund.

The Studio companion protocol changes in the Hailuo checkout are required for
short acknowledgements, requested-second billing metadata, automatic empty-media
confirmation, and verified archive delivery. They have not been deployed by this
local update. No production database or running service is modified.

Build outputs and verification manifests belong to the parent `artifacts/`
directory, not Git. Verification completed on SQLite 3.50.4, MySQL 8.4.11,
and PostgreSQL 18.6: new databases, databases created by the actual rc.47
source, two initialization/migration passes, preserved wallet/token balances
and image task data, and unique H3 reservations. MySQL/PostgreSQL also used
separate disposable log databases. Commands and logs are retained in
`artifacts/verification-scripts/` and `artifacts/migration-matrix-*.json`.

`go test ./service -run H3 -count=1` passed on all three engines using
`H3_TEST_MYSQL_DSN` or `H3_TEST_POSTGRES_DSN` for dedicated test databases.
The full model/service suites and focused H3/image/controller/middleware tests
passed. Frontend type checking, 196 related tests, lint, and production build
passed. The complete controller suite hit its default ten-minute timeout;
it is not recorded as passing or established as an upstream baseline issue.

Production readiness still requires a separately authorized deployed end-to-end
test and an operating configuration compatible with the H3 billing boundary.
Local tests do not prove live GPU generation.
