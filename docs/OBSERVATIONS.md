# Observations

> **Classification:** Current — Things noticed about this codebase and its
> dependencies that are not fixed yet, with the evidence and what would close
> each one.

> **Last verified:** 2026-09-29

A finding that is not acted on in the change that found it is written down
here, so it does not live only in a chat or a commit message. Each entry says
what was seen, why it matters, how to check it again, and what would close it.
When one is closed, it moves to [Closed](#closed) with the commit that closed it.

This file is published with the public repository. An unfixed vulnerability
someone could exploit, or anything naming the hosted service's infrastructure,
is recorded instead in `docs/OBSERVATIONS_PRIVATE.md`, which the public export
leaves out, until it is fixed.

## Open

### SheetJS is outside Dependabot's and npm audit's view

- **Noticed:** 2026-09-27, while closing the public repository's Dependabot alerts (`0a74995`).
- **What:** `web/package.json` installs `xlsx` from the vendor's CDN,
  `https://cdn.sheetjs.com/xlsx-0.20.3/xlsx-0.20.3.tgz`. The npm registry copy
  stops at 0.18.5 and never received the fixes for prototype pollution
  (CVE-2023-30533) and ReDoS (CVE-2024-22363), so the registry version cannot be
  used. Neither Dependabot nor `npm audit` tracks a tarball URL: a future SheetJS
  advisory raises no alert.
- **Why it matters:** `web/src/consoles/developer/ImportWizard.tsx` parses the
  workbook a developer chooses, in the browser (`XLSX.read`, `sheet_to_json`).
  `web/src/consoles/business/ImportWidget.tsx` only writes the template workbook.
- **How to check:** compare the installed version with the vendor's latest —
  `curl -sL https://cdn.sheetjs.com/xlsx-latest/package/package.json | jq -r .version`
  against the version in `web/package.json` — and search the
  [GitHub Advisory Database](https://github.com/advisories?query=sheetjs) for
  SheetJS. Do it at every dependency review.
- **What closes it:** SheetJS publishing fixed versions to npm again, or the
  wizard handing the file to the gateway, which already parses uploaded `.xlsx`
  with excelize (`internal/importpkg/xlsx.go`), and the browser parser going.

### Three tool images are compiled with an unsupported Go

- **Noticed:** 2026-09-27, same review.
- **What:** `deploy/docker/pg-backup/Dockerfile` (`mc`),
  `deploy/docker/minio/Dockerfile` (the MinIO server) and
  `deploy/docker/postgres-walg/Dockerfile` (`wal-g` and `mc`) build those tools
  from source `FROM golang:1.25`. Go supports only the two newest releases,
  1.27.x and 1.26.x as of this date (`https://go.dev/dl/?mode=json`), so 1.25 no
  longer gets security fixes and these binaries keep whatever standard-library
  flaws its last release had.
- **Why it matters:** the backup and point-in-time-recovery images hold the
  database and object-store credentials. No scanner covers this: Dependabot and
  `govulncheck` look at this repository's own Go module, not at third-party
  sources compiled inside a Dockerfile.
- **How to check:** `grep -rn "FROM golang" deploy/docker/`, against the
  supported releases at `https://go.dev/dl/?mode=json`.
- **What closes it:** raising the three Dockerfiles to a supported Go (1.26 or
  1.27), building each image — the publish job does, or
  `docker build deploy/docker/<name>` — and running a backup and a restore
  (see Backups and Restoring in [SELF_HOSTING.md](SELF_HOSTING.md)).

### Nine Go files are not gofmt-formatted

- **Noticed:** 2026-09-27.
- **What:** `gofmt -l cmd internal pkg` lists `internal/identity/jwks.go`,
  `internal/importpkg/gsheets.go` and seven files in `internal/integration/`
  (struct fields not aligned). CI's `Go lint` job passes regardless, so the
  lint configuration does not enforce gofmt.
- **Why it matters:** only noise today, but every later edit to those files
  carries unrelated formatting changes in its diff.
- **How to check:** `gofmt -l cmd internal pkg`.
- **What closes it:** `gofmt -w` on those files, and enabling the `gofmt`
  formatter in the golangci-lint configuration so it cannot drift again.

### The console is one 1.75 MB JavaScript file

- **Noticed:** 2026-09-27; `vite build` warns about chunks over 500 kB.
- **What:** the production build emits a single entry chunk of about 1.75 MB
  (`web/dist/assets/index-*.js`), with no route or console-section splitting.
- **Why it matters:** every first visit downloads and parses all of it, whichever
  role the person holds and whichever screen they open.
- **How to check:** `cd web && npx vite build`, then the size of
  `dist/assets/index-*.js`.
- **What closes it:** loading the heavier screens lazily (for example the
  developer console, the workflow canvas, the import wizard with SheetJS, and
  charts) so the entry chunk falls under Vite's warning.

### One known advisory has no fix and is not reachable

- **Noticed:** 2026-09-27.
- **What:** `govulncheck ./...` reports GO-2026-5932 — `golang.org/x/crypto/openpgp`
  is unmaintained — as a vulnerability "in modules you require, but your code
  doesn't appear to call". No release fixes it; nothing here imports that package.
- **How to check:** `go run golang.org/x/vuln/cmd/govulncheck@latest -show verbose ./...`;
  the "Symbol Results" section must stay empty.
- **What closes it:** nothing to do unless a package here starts importing
  `openpgp`. It is listed so nobody chases it again.

### Imported property names are not checked, so some cannot be used in formulas

- **Noticed:** 2026-09-27, while adding the property-name rule (contract C9).
- **What:** the developer's property POST/PATCH now goes through
  `metricformula.ValidatePropertyDeclaration`: the name must match
  `[A-Za-z_][A-Za-z0-9_]*`, be unique on the dimension regardless of case, and
  have type `text`, `number` or `date`. The CSV member import
  (`importDimensionMembersCSV` in `internal/gateway/handler.go`) and the REST
  connector's dimension commit (`commitDimension` in
  `internal/integration/commit.go`) still auto-declare whatever follows
  `property:` in a header, as `text`, with `ON CONFLICT (dimension_id, name)`.
  That conflict check is case-sensitive, so an import can also declare
  `Segment` next to an existing `segment`.
- **Why it matters:** a property named `Sales Segment` or `unit-price` holds
  values but cannot be written as `dimension.property`. Two declarations that
  differ only in case make the formula's reading ambiguous: the scheduler takes
  the first one in sorted order.
- **How to check:** import a dimension CSV with a `property:Sales Segment`
  column, then list `GET /api/developer/dimensions/{id}/properties`.
- **What closes it:** a decision on imports. Either refuse a non-identifier
  header, or map it to an identifier and keep the header as a label. Either
  way, reuse the existing declaration when only the case differs.

### Removing a metric from a grid, or a time dimension, is only partly checked

- **Noticed:** 2026-09-27, while extending the placement checks to LOOKUP and
  the conditional aggregations.
- **What:** `ValidateGridTime` runs when a grid **gains** a metric or a
  dimension (`gridMembershipTx`). It re-checks `DIMENSION_NOT_ON_SOURCE`,
  `CONFLICTING_DIMENSION_ARGUMENTS` and `TIMESUM` period codes, for dependents
  on other grids too. Removing a **dimension** from a grid
  (`DELETE /api/developer/grids/{id}/dimensions/{dim}`) now runs the same
  dimensional checks (`metricformula.ValidateGridDimensional`) and is refused
  with `DIMENSION_NOT_ON_SOURCE` when a LOOKUP or criteria range still reads
  one of the grid's metrics along it. Two removals are still unchecked:
  - removing a **metric** from its grid leaves it unplaced, so its
    dimensions become unknown and every dimensional check on it is skipped;
  - the **time** rules (`TIME_DIMENSION_REQUIRED`, the shared recurrence
    axis) are not re-run on any removal.
- **Why it matters:** a formula saved against a placed source keeps
  computing once the source is detached, reading whatever the runtime
  normalisation makes of the unplaced source; and a time-series metric can
  lose its time axis. Activation (`ValidateTime`) catches the time case, but a
  working revision can compute wrong numbers until then.
- **How to check:** put a `LAG(x, 1)` metric on a grid with a time dimension,
  then `DELETE` the time dimension from the grid. It returns 200.
- **What closes it:** deciding whether removals should refuse or only warn for
  the time rules, then running `ValidateGridTime` on them as well; for an
  unplaced source, deciding whether a dimensional read of it is an error.
- **Also:** since 2026-09-28 the AI Developer's `remove_grid_metric` and
  `remove_grid_dimension` mirror these endpoints, so they share the gap and
  close with it.

### Dimension and member renames leave formulas naming the old name

- **Noticed:** 2026-09-27, same change.
- **What:** member create, edit and delete, property rename, retype and
  delete, and member imports now recompute the metrics whose formula reads the
  dimension (contract C8). A property rename also moves every member's value
  and any `source_property` to the new name, and since 2026-09-28 rewrites
  `dim.old` to `dim.new` in every formula of the revision
  (`formula.RenameProperty`, `metricformula.RenamePropertyInFormulas`). Two
  changes still do not rewrite formula text:
  - a dimension rename (`PATCH /api/developer/dimensions/{id}`), which also
    triggers no recalculation;
  - a member code rename, which re-keys facts and results but not a literal
    `LOOKUP(x, region, "EMEA")`.
- **Why it matters:** after a member rename, the formula fails visibly at the
  next recalculation with `#N/A`. After a dimension rename it keeps its old
  values until something else recomputes it.
- **How to check:** rename a dimension that a formula uses as a bare name, and
  see that nothing recomputes.
- **What closes it:** the same token-level rewrite the property rename now
  uses, extended to bare dimension names, `dim.property` prefixes, LOOKUP and
  criteria dimension arguments and literal member codes of that dimension.

### Deleting a property leaves its values on the members

- **Noticed:** 2026-09-27, same change.
- **What:** `DELETE /api/developer/dimensions/{id}/properties/{propId}`
  removes the declaration only. Every member keeps the key in its `properties`
  JSON, and declaring the same name again brings the old values back. Member
  property values are also never checked against the declared `data_type`: a
  `number` property can hold `"abc"`, which a formula reads as `#VALUE!`.
- **Why it matters:** old data comes back silently when a property is
  re-declared, and a type error only surfaces when a formula evaluates it.
- **How to check:** delete a property, then
  `SELECT properties FROM model.dimension_member WHERE dimension_id=...`.
- **What closes it:** removing the key in the delete transaction (the rename
  path already rewrites keys, in `metricformula.RenamePropertyValues`), and
  checking values against `data_type` on member PATCH and on imports.

### Dependents of dependency-free metrics are missed by the recalculation from inputs

- **Noticed:** 2026-09-27, while wiring the dimension recalculation triggers.
- **What:** `RecalcAffected` walks outwards from the changed inputs, then
  `withDependencyFree` (`internal/calculation/scheduler.go`) adds every
  calculated metric that has no edges, such as `COUNTIFS(region, "*")`. It does
  not add the dependents of those metrics: `m2 = m1 * 2`, where `m1` has no
  edges, is reached neither from an input nor by `withDependencyFree`.
  `recalcAllInputsAcrossRevisions` and `recalcRevisionFromInputs` (revision
  duplication and import) inherit this.
- **Why it matters:** `m2` keeps a stale value, or none after a duplication,
  until its own formula is saved again. The new
  `Scheduler.RecalcDimensionDependents` and the AI-promotion recalculation
  include dependents, but the older paths do not.
- **How to check:** save `m1 = COUNTIFS(region, "*")` and `m2 = m1 * 2`,
  duplicate the revision, and read `m2`'s rows in the copy.
- **What closes it:** `withDependencyFree` also adding
  `AffectedMetricIDs(defs, dependencyFree)`.

### Model import and the gRPC CreateMetric skip formula validation

- **Noticed:** 2026-09-27, while mapping every path that writes a formula.
- **What:** `internal/modeltransfer` import (`POST /api/admin/models/import`
  and the sign-up starter) inserts formulas and trusts the package's
  `calc_dependency` edges without running `metricformula.Validate` or
  `ValidateTime`. `internal/model`'s gRPC `CreateMetric` resolves references
  with `ExtractFormulaRefs`, which records no offsets and logs its errors. It
  is reachable only from `cmd/verify-topology`.
- **Why it matters:** a package that is older or edited by hand can carry
  things the save path now refuses: an undeclared property, an unknown literal
  member, a LOOKUP along a dimension its source does not have, or a
  dimensional call inside a recurrence. Its time-dimension edges can also be
  bounded where they should be unbounded. The scheduler still refuses the
  recurrence at run time, but the other cases compute or fail per cell.
- **How to check:** edit an exported package's formula to
  `LOOKUP(x, region, "NOPE")` and import it. The import succeeds.
- **What closes it:** re-deriving each imported metric's edges with `Validate`
  inside the import transaction and running `ValidateTime` before commit, and
  routing or retiring the gRPC path.

### The connector lower-cases property headers; the CSV import keeps their case

- **Noticed:** 2026-09-28, while wiring the dimension recalculation triggers.
- **What:** `commitDimension` (`internal/integration/commit.go`) lower-cases
  every header before splitting off `property:`, so a connector column
  `property:Segment` declares and writes `segment`. The gateway's CSV member
  import (`importDimensionMembersCSV`) keeps the header as written and
  declares `Segment`.
- **Why it matters:** the same property can land under two JSONB keys
  depending on the import path. Formulas match property names regardless of
  case, but member values are stored under the exact key, so one path's
  values are invisible to a declaration made by the other.
- **How to check:** pull `property:Segment` through a REST connector and
  import the same header by CSV into another dimension, then compare
  `model.dimension_property.name`.
- **What closes it:** one header normalisation shared by both importers,
  decided together with the "Imported property names are not checked" entry.

### Two concurrency gaps in the dimension recalculation and property rename

- **Noticed:** 2026-09-28, by the stage 2b verifier; neither is proven.
- **What:** (1) The property PATCH runs
  `metricformula.ValidatePropertyDeclaration` before its rename transaction,
  and the table's unique key `(dimension_id, name)` is case-sensitive, so two
  concurrent renames to `Segment` and `segment` can both pass the check.
  (2) A member DELETE starts `recalcAllInputsAcrossRevisions` and
  `recalcDimensionDependents` as two goroutines over the same revision, so
  both may write the same metric's `calc_result` rows at once.
- **Why it matters:** (1) leaves two declarations that differ only in case,
  which formulas then read ambiguously. (2) is harmless while both passes
  compute from the same committed definitions, but the last writer wins if
  they ever disagree.
- **How to check:** (1) fire two such PATCHes in parallel and count
  `lower(name)` duplicates. (2) delete a member of a dimension read by a
  metric that also has fact inputs, and log the order of both passes' writes.
- **What closes it:** (1) the uniqueness check inside the rename
  transaction, or a unique index on `(dimension_id, lower(name))` in a new
  migration. (2) running the two passes in sequence in one goroutine.

### A time-dimension member change recalculates only revisions with fact data

- **Noticed:** 2026-09-28, by the stage 2b verifier; not proven.
- **What:** member POST, PATCH and DELETE on a time dimension recalculate
  through `recalcAllInputsAcrossRevisions`, not `recalcDimensionDependents`.
  That pass visits only revisions that have fact data.
- **Why it matters:** a revision without facts that has a metric such as
  `COUNTIFS(month, "*")` keeps its old value after a period is added or
  removed.
- **How to check:** in a revision with no facts, save
  `COUNTIFS(month, "*")`, add a period member, and read the metric's rows.
- **What closes it:** also calling `recalcDimensionDependents` from the
  time-dimension branches, the way the non-time branches do.

### Read sets over-withhold through dimension relations

- **Noticed:** 2026-09-28, while building `internal/readset` (contract C7).
- **What:** a served metric's read set records a read through a *related*
  dimension (a LOOKUP or criteria range on `region` over a source keyed by
  `employees`, a plain reference resolved through a parent-dimension chain)
  as reading every leaf of the source's dimension, as the contract's table
  says. A member-local expression at an aggregate cell is evaluated at the
  aggregate and at every leaf under it. A recurrence extends every time
  window of the footprint to the first period (a backward loop) or the last
  (a forward one), not only the windows of the loop itself.
- **Why it matters:** fail-closed, never a leak, but a restricted viewer may
  see "withheld" for a value that reads only visible members.
- **How to check:** `SUMIFS(salary, region, "EMEA")` over `salary[employees]`
  with `region` a property grouping of `employees`; hide one employee outside
  EMEA; the cell is withheld.
- **What closes it:** resolving the relation's members in the read set (the
  way `rollup.relate` does at run time) instead of reading every leaf.

### LOOKUP of an aggregate period persists 0 for a time_summary none source

- **Noticed:** 2026-09-28, stage 2 review.
- **What:** `LOOKUP(x, month, "FY26")` where `x.time_summary = none` has no
  value at the aggregate. The resolver reports `found = false`, and LOOKUP
  returns 0 rather than blank. The `*VALUE` functions return blank in the
  same case.
- **Why it matters:** a persisted 0 reads as a real number in the grid.
- **How to check:** create the metric above and read the persisted row.
- **What closes it:** LOOKUP returning blank when nothing was found at an
  aggregate period of a `none` source, with the scheduler leaving no row.

### Scoped grid reads may read a formula/rate dependency as a mean of children

- **Noticed:** 2026-09-28, final review of the dimensional-references change,
  while fixing the scheduler's read of a formula/rate source at partial
  coordinates. Seen in the code, not reproduced over HTTP.
- **What:** the scheduler now evaluates a formula/rate source on demand where
  it has no persisted row (`internal/calculation/ondemand.go`). The gateway's
  scoped recompute (`scopeOrdinaryMetric` in `internal/gateway/scoped_calc.go`)
  still reads an ordinary metric's calculated dependency through
  `rollup.ResolveTime` with the dependency's own `agg_rule`. For a dependency
  whose rule is `formula` or `rate`, a combination above its leaves then
  combines the children by `combineAgg`, which answers their mean.
- **Why it matters:** a restricted viewer's cell of `x = margin * 2` at a
  parent, or on a grid with fewer dimensions than `margin`, could differ from
  the value everyone else sees.
- **How to check:** a `margin = profit / revenue` metric (`agg_rule` formula)
  on [region, product], `x = margin * 2` on [region]; hide one member from a
  viewer and compare the viewer's `x` at a region with
  `2 * (sum of profit) / (sum of revenue)` over the viewer's visible products.
- **What closes it:** the scoped recompute evaluating such a dependency's
  formula at the combination (as it already does for the metric's own rollup
  cells), or reading the persisted row when the viewer's read set allows it.

### A time-series formula/rate source cannot be read at partial coordinates

- **Noticed:** 2026-09-28, final review.
- **What:** a LOOKUP, plain reference or `*VALUE` read of an `agg_rule`
  formula/rate source at coordinates with no persisted row is evaluated on
  demand, except when the source itself calls a time function: that read
  fails with a message saying so (`exactSource` in
  `internal/calculation/ondemand.go`), rather than using a mean of children.
- **Why it matters:** such a formula fails on those cells instead of
  computing.
- **How to check:** `LOOKUP(ratio_ts, region, "World")` from a cell with fewer
  dimensions than `ratio_ts = PREVIOUS(a) / b` (`agg_rule` formula).
- **What closes it:** evaluating a time-series source at such coordinates
  (the time-series evaluator at a partial combination), or persisting every
  partial combination for it.

### Every member edit now recalculates the metrics placed on the dimension

- **Noticed:** 2026-09-28, when a grid by a property grouping did not follow
  a source member's property edit.
- **What:** `RecalcDimensionDependents` recalculated only metrics whose
  formula NAMES the dimension (or a related one). A calculated metric placed
  on a grid with the grouping (`area_sal = salary` on a grid by `area`) reads
  through it without naming it, and kept its old values. It now also
  recalculates every calculated metric placed on a grid with the dimension or
  a related one.
- **Why it matters:** correct, but a member add/edit/delete on a large
  dimension now recalculates every calculated metric on its grids, not only
  the few whose formulas name it.
- **How to check:** `TestPropertyGroupingDimensionOverHTTP` (the Teams-style
  grid follows the edit); time a member edit on a large model.
- **What closes it:** narrowing to metrics whose values can change — those
  that read a metric on a related dimension — if the cost shows up.

### Recalculation passes are serialized only within one process

- **Noticed:** 2026-09-28, making a metric's result set replace atomically.
- **What:** `lockRevision` (`internal/calculation/scheduler.go`) serializes
  passes per revision with an in-process mutex, but the gateway and the
  `cmd/calculation` NATS worker each run a `Scheduler`. Two passes over the
  same metric in different processes replace its rows in overlapping
  transactions; under READ COMMITTED the second one's DELETE does not see
  rows the first commits after that DELETE began, so both sets can survive
  (duplicate per-combo rows), or the two DELETEs can deadlock and one pass
  fails (keeping the last good rows).
- **Why it matters:** readers that do not pick one row per combination could
  combine two passes' values; before the transaction the same overlap
  interleaved statement by statement, so this is not new.
- **How to check:** run a recalculation of one revision from the gateway and
  from the worker at the same time, then count `runtime.calc_result` rows per
  (metric, `dim_members`).
- **What closes it:** a cross-process lock per revision
  (`pg_advisory_xact_lock` on the revision inside `Store.InTx`), or a unique
  key on (revision, metric, `dim_members`) with an upsert.

### The formula-lookups harness has not been run against a live gateway

- **Noticed:** 2026-09-28, final review.
- **What:** `cmd/verify-formula-lookups` builds and vets, but its first run
  against the e2e gateway (`http://localhost:8081`, database `mavericks_e2e`)
  stopped at the bootstrap: that database has no users, so there is no
  platform administrator to create the tenant as (`GET /api/admin/tenants`
  answers 401). The first administrator of an install is created by
  `scripts/bootstrap-platform-admin.sh`, not over HTTP.
- **Why it matters:** none of the harness's assumptions have been seen live.
- **How to check:** `go run ./cmd/verify-formula-lookups` against the e2e
  gateway.
- **What closes it:** giving `mavericks_e2e` its first platform administrator
  once (`scripts/bootstrap-platform-admin.sh` pointed at it) and a green run.

### MEMBER_IN_USE finds only literal member names

- **Noticed:** 2026-09-28, adding `MEMBER_IN_USE`.
- **What:** `metricformula.CheckMemberNotInUse` refuses a member delete for
  the forms listed in `FORMULA_CALCULATION_INSTRUCTIONS.md` (literal LOOKUP
  member, literal `TIMESUM` code on the grid's time dimension, equality
  criterion on the code range, `=`/`<>` between the bare dimension or
  `PARENT(dim)` and a literal, and — since the final-review fix round — a
  `SWITCH` on the bare dimension or `PARENT(dim)` with a literal match
  value). It does not see a code computed at run time
  (`LOOKUP(x, zone, zone.alt)`, `IF(LEFT(zone, 1) = "Z", ...)`), an ordering
  (`zone > "Z1"`), a wildcard criterion, or a criterion on a `dim.property`
  range whose value happens to equal a code. Like the property and dimension
  checks, it reads the formulas before the delete without locking them, so a
  formula saved between the check and the delete is not seen.
- **Why it matters:** those formulas keep the pre-existing behaviour: a cell
  that reads the deleted member fails with `#N/A` (rows cleared by the
  scheduler's missing-member backstop), or a comparison silently stops
  matching.
- **How to check:** save `IF(LEFT(zone, 1) = "Z", 1, 0)`, delete Z2: 200.
- **What closes it:** nothing needed for computed codes (no static answer
  exists); the race closes with a lock shared by formula saves and deletes.

### `grid_def.rollup_source_grid_id` can only be set by a model import

- **Noticed:** 2026-09-28, AI Developer parity audit.
- **What:** a grid that mirrors another grid's metrics through a
  cross-dimension rollup is marked by `model.grid_def.rollup_source_grid_id`.
  Revision copies and model export remap it, and the grid reader honours it,
  but no developer endpoint or screen sets or clears it: the only writer is
  `internal/modeltransfer` (model import, a tenant-admin action).
- **Why it matters:** standing rule 2. The project's own guidance points at it
  as a primitive to reuse, yet the developer role cannot reach it, and so the
  AI Developer cannot either.
- **How to check:** `grep -rn rollup_source_grid_id internal/gateway` finds
  only reads and the revision-copy remap.
- **What closes it:** a field on `PATCH /api/developer/grids/{id}` (same
  model and revision, no cycles) with a control on the Grids screen, then the
  same on the assistant's `update_grid`.

### Renaming or deleting a business role leaves workflow steps naming it

- **Noticed:** 2026-09-28, AI Developer parity audit.
- **What:** workflow steps name business roles by NAME (`assignee_roles`,
  `recipient_role`). `PATCH` and `DELETE /api/business-admin/roles/{id}`
  change or remove the role without touching the steps, so the steps then
  name no role. The assistant's `update_business_role` and
  `delete_business_role` do the same but list the workflows affected; the
  console says nothing.
- **Why it matters:** a published workflow's approval step assigned to a
  renamed role waits for a role nobody holds, and the start dialog does not
  warn.
- **How to check:** assign a step to role "Finance Review", rename the role on
  the Roles screen, and validate the workflow: the step still names the old
  name.
- **What closes it:** carrying a rename into the steps of the application's
  workflow definitions (or refusing it while steps name the role), and a
  warning before a delete — in the endpoint, so both doors get it.

### Developers can manage role membership through the API but not on a screen

- **Noticed:** 2026-09-28, adding the developer's Roles tab.
- **What:** the `baOrDev` guard covers every `/api/business-admin/roles`
  route, including `POST .../members` and `DELETE .../members/{userId}`. The
  developer's Roles tab (and the assistant) create, rename and delete roles
  and set their dashboards, but leave membership to a business admin, as the
  documentation says.
- **Why it matters:** a guard wider than any screen that uses it; the
  guidance is a narrow, purpose-scoped guard.
- **How to check:** as a developer without `business_admin`,
  `POST /api/business-admin/roles/{id}/members` answers 200.
- **What closes it:** deciding whether developers manage membership; if not,
  `ba()` on the two member routes.

### The developer's folder and dashboard updates overwrite fields they were not sent

- **Noticed:** 2026-09-28, AI Developer parity audit.
- **What:** `PATCH /api/developer/folders/{id}` sets `parent_id` from the body
  every time, so a rename without `parent_id` moves the folder to the top
  level; `PATCH /api/developer/dashboards/{id}` requires a name and replaces
  the tags, so an update without `tags` clears them. `POST` and `PATCH` on
  folders also accept any existing folder as `parent_id`, of any model. The
  console always sends every field, so it is not affected; the assistant's
  folder and dashboard tools update only what they are given and check the
  folder's model.
- **Why it matters:** an API client making a partial update loses data, and a
  folder can be filed under another model's folder (and then vanish from its
  own tree).
- **How to check:** `PATCH` a nested folder with only `{"name": "x"}` and read
  its `parent_id`.
- **What closes it:** presence-aware updates, as the metric `PATCH` does, and
  the same-model check `validateDashboardFolder` makes for dashboards.

### `list_workflow_roles` lists every workspace's roles; the role tools use one

- **Noticed:** 2026-09-28, AI Developer parity audit.
- **What:** the assistant's `list_workflow_roles` lists the business roles of
  every workspace of the application's customer, while `create_business_role`,
  `update_business_role`, `delete_business_role` and `set_role_dashboards`
  resolve roles in one workspace, as the Roles screens do.
- **Why it matters:** in a tenant with several workspaces the assistant can
  be shown a role it cannot then change ("not found in this workspace").
- **How to check:** a customer with two workspaces, a role in each; the list
  shows both, `update_business_role` on the other one fails.
- **What closes it:** the list scoped to the same workspace, or roles resolved
  across the customer's workspaces everywhere.

### The member-delete confirm and the manual say descendants are deleted

- **Noticed:** 2026-09-28, adding `MEMBER_IN_USE`.
- **What:** the console's "Delete member?" dialog (`DimensionsTab.tsx`) says
  it removes the member "and its N descendants", and developer manual part
  20 says "Deleting a member deletes its descendants". The foreign key
  `dimension_member.parent_member_id` is `ON DELETE SET NULL`
  (`migrations/018`), and `deleteMemberReindexed` deletes one row, so the
  children stay, as roots. Seen by reading; not yet exercised live.
- **Why it matters:** a developer expecting a subtree delete is left with
  orphaned roots, and a formula naming a child's code is not checked
  because the child is not deleted (correctly).
- **How to check:** delete a parent member with children over HTTP and list
  the dimension's members.
- **What closes it:** deciding which is intended, then aligning the dialog
  and manual text, or the delete, with it.

### Definition names are unique by exact case, formulas resolve them regardless of case

- **Noticed:** 2026-09-28, mapping the unique violation to
  `DIMENSION_NAME_TAKEN` / `METRIC_NAME_TAKEN`.
- **What:** `dimension_def_*_uq` and `metric_def_*_uq`
  (`migrations/027`) are on `name`, not `lower(name)`, so `Region` can be
  created next to `region` in one revision. Formulas match dimension and
  metric names case-insensitively. Seen by reading; not exercised live.
- **Why it matters:** which of the two a formula reads is then decided by
  lookup order, not by the author.
- **How to check:** `POST /api/developer/dimensions {"name":"Region",
  "revision_id":<a revision with region>}`.
- **What closes it:** a case-insensitive name check in the create and rename
  paths (developer and AI), answering the same 409 codes.

### Count leaves out a recorded 0, and averages take in a formula's 0 rows

- **Noticed:** 2026-09-28, making every reader combine a parent flat; the
  browser half found by the cross-reader verification of that change.
- **What:** two readings of "a leaf that has a recorded value" are open.
  - Count: `agg_rule` count at a parent is the number of leaves whose value
    is recorded **and not 0** (`rollup.CombineAgg`, which the scheduler's
    totals and slice rows use). The grid client now applies the same rule
    (`combineFlat` in `PlanningGrid.tsx`); it had counted every present
    cell, so every calculated count parent differed from the server
    (ccnt World February 5 against 3). A typed 0 at an input leaf is
    therefore not counted.
  - Average: the scheduler writes a 0 row wherever a calculated metric's
    formula saw no inputs (a leaf with no data, or one month without it).
    A member-dependent calculated average (`iavg * region.factor`) then
    averages those 0 rows in, while an input average leaves the same leaf
    out: World February 11 = (4+0+16+35+0)/5 against the input's 4.333. All
    server readers and the grid agree on it. A restricted viewer's total
    differs from the developer's accordingly (20.25 against 16.2 with MX
    hidden), because the hidden leaf's 0 row drops out.
- **Why it matters:** the decision behind the flat rule was worded "the
  number of leaves that have a recorded value" and "the mean of all leaves
  under the coordinate that have a recorded value"; whether a formula's 0 at
  an intersection with no inputs is a recorded value decides both numbers.
- **How to check:** `TestResolveAverageAndCountAreFlatOverLeaves` (FR
  recorded as 0 counts 2 at EMEA); the Playwright case "a calculated leaf is
  its own value; count skips zeros; a pure ratio is the server's".
- **What closes it:** a user decision. Either confirm the non-zero count and
  the 0-row average as they are, or have the scheduler write no row where
  every input is missing (then count can count every recorded value,
  `CombineAgg` and `combineFlat` both change, and averages leave the no-data
  leaves out).

### A pure-ratio average metric has no rows between its leaves and its total

- **Noticed:** 2026-09-28, cross-reader verification of the flat-parent
  change.
- **What:** a calculated metric with agg_rule `average` whose formula does
  not depend on the members is evaluated at the aggregate by the scheduler.
  A non-served one collapses to one evaluation (`executePartition`) and has
  no leaf rows at all; a served one (`PREVIOUS(revenue)`) has leaf rows. Both
  persist only one-dimension slice rows and the `'{}'` total above the
  leaves, never two-dimension rollup combos (World × February). The grid
  (flag `aggregate_evaluated`) now takes such a metric's parent cells from
  the server's rows, so a parent row that is not the whole grid renders "—"
  (it used to render the leaf-cell mean, a number no server reader gives),
  and a non-served one also shows "—" at every leaf. Chart-data reads the
  same rows: EMEA × February has no point, World × February is the
  `{period: February}` slice (a pin on a sole root is dropped).
- **Why it matters:** a business user sees blanks where a scoped KPI shows a
  number.
- **How to check:** `TestPureRatioAverageReadsTheSchedulersAggregate`
  (`internal/gateway`): EMEA × 2026-02 of `prva` has no row.
- **What closes it:** the scheduler persisting the rollup combos for this
  rule as it does for formula and rate (`RollupCombos` in
  `tsEvaluator.finish` and `executePartition`), and the collapsed case
  writing its leaf rows, or a product decision that such a metric has no
  per-intersection value.

### Chart-data now evaluates sum and count calculated metrics at every leaf

- **Noticed:** 2026-09-28, making every reader combine a parent flat.
- **What:** a chart point of a calculated metric whose `agg_rule` is `sum`
  or `count` (or a member-dependent `average`) evaluates the formula at each
  leaf under the point and combines the leaves, as the scheduler does
  (`leafWise` in `internal/query/chart.go`). Before, `sum` and `count`
  evaluated the formula once over aggregated inputs, which is wrong unless
  the formula is linear in sums (`iavg * 2` summed is not twice the
  average). The cost of a point is now proportional to the number of leaf
  combinations under it.
- **Why it matters:** a chart of a large model with nothing pinned could be
  slower than before. Not measured on a large model.
- **How to check:** time `/api/dashboard-widgets/{id}/chart-data` for a chart
  of a `sum` calculated metric on the sales demo with no context pinned,
  against the previous build.
- **What closes it:** a measurement; if slow, serving such points from the
  scheduler's persisted leaf and slice rows (`servedFetch`) where the
  viewer's read set allows it.

### An unauthenticated /api/grid answers 500 instead of 401/404

- **Noticed:** 2026-09-28, checking production after the formula deploy
  (8c04baa). This predates that change: `grid()` is unchanged from 0a74995.
- **What:** `grid()` looks up the model before resolving the caller. Without
  a token, `GET /api/grid` answers 500 `resolve model: missing Bearer token`.
  With an unknown `grid_def_id`, it answers 500 `resolve grid def: no rows in
  result set`. With an existing one, it answers 401. Every other endpoint
  answers 401 first.
- **Why it matters:** the status codes are wrong, 500 alerts fire on normal
  traffic, and an unauthenticated caller can tell an existing grid ID (401)
  from an unknown one (500). IDs are random UUIDs and no data is served, so
  this is not a leak.
- **How to check:** `curl -s -w '%{http_code}' https://app.maverickbuilds.app/api/grid`.
- **What closes it:** resolve the actor first (401), and answer an unknown
  grid with 404.

## Closed

### The Add metric form screenshot shows the old `{name}` hint

- **Noticed:** 2026-09-29, when the Add metric form moved to the row editor's
  name check.
- **What:** `docs/developer-manual/img/20-metric-add-form.png` still showed the
  form's former hint and placeholder (`use {metric_name} references`,
  `{hc_cost} + {software_cost}`); the chapter's text described the new form.
- **Why it matters:** cosmetic: the picture disagreed with the text beside it.
- **How to check:** open the image next to the Metrics chapter.
- **What closes it:** a new capture of the form.
- **Closed:** 2026-09-29. Recaptured on the Simple Budget Tutorial model (the
  OPEX Planning 2026 demo the older captures show was deleted from the local
  development database at the owner's request), with a typo in the formula so
  the new hint is visible: "Not a metric or dimension in this revision:
  varience".

### Deleting a metric leaves the formulas that read it failing

- **Noticed:** 2026-09-28, proved live by `cmd/verify-formula-catalogue`
  (`tmp_dep = tmp_src * 2`, then delete `tmp_src`).
- **What:** `DELETE /api/developer/metrics/{id}` deleted the metric, cascaded
  its `calc_dependency` rows and recalculated its former dependents, but their
  formula text still named it: `tmp_dep` stayed `=tmp_src * 2` and failed at
  all 36 cells with `#NAME?: Unknown name: tmp_src`, keeping its last results.
  The console's confirmation said "This removes the metric from all formulas
  that reference it". A Rate total whose operand was deleted silently lost it
  (`agg_numerator_metric_id` is `ON DELETE SET NULL`).
- **Why it matters:** a developer trusting the dialog broke every dependent
  metric, which then kept serving stale numbers with only a warning icon.
- **How to check:** `go run ./cmd/verify-formula-catalogue` (the two
  `tmp_src` checks).
- **What closes it:** refusing the delete while the metric is read.
- **Closed:** 2026-09-29 (uncommitted), by the user's decision: a metric used
  by formulas cannot be deleted until they are cleaned.
  `metricformula.CheckMetricNotInUse` refuses the delete with `METRIC_IN_USE`
  (HTTP 409) while another metric's formula reads it (parsed; an unparsable
  formula matched as text) or it is another metric's Rate numerator or
  denominator, naming them; on the developer delete and the AI's
  `delete_metric` (the model store's `DeleteMetric` has no caller). The
  console's confirmation names the readers and the refusal shows under the
  row. Proved by `TestCheckMetricNotInUse`, live (409 naming `tmp_dep` and
  `tmp_rate (its Rate total)`, then 200 once cleaned) and in the running
  console.

### Name uniqueness regardless of case is enforced by triggers

- **Noticed:** 2026-09-28/29, with migrations `100_metric_name_case.sql` and
  `101_dimension_name_case.sql`.
- **What:** metric and dimension names were unique regardless of case through
  `BEFORE INSERT OR UPDATE` triggers, chosen so a database that already holds
  a pair such as `Sales` beside `sales` keeps starting.
- **Why it matters:** a trigger is a weaker guarantee than an index, and a
  legacy revision holding a pair cannot be copied until one is renamed.
- **How to check:** on each deployment,
  `SELECT model_id, revision_id, lower(name), count(*) FROM model.metric_def GROUP BY 1, 2, 3 HAVING count(*) > 1;`
  and the same over `model.dimension_def`.
- **What closes it:** unique indexes on `lower(name)` where no pair exists.
- **Closed:** 2026-09-29 (uncommitted). Checked read-only on 2026-09-29:
  production (shared database: 0 metrics, 0 dimensions; the tenant database:
  3 metrics, 2 dimensions) and staging (12 metrics, 6 dimensions) hold no
  pair. Migrations 100 and 101 (never released, so rewritten) now build unique
  indexes `metric_def_name_ci_uq` / `metric_def_null_rev_name_ci_uq` and the
  dimension equivalents; only a database that already holds a pair (a
  self-hosted install, say) gets the trigger instead, with a WARNING, so it
  keeps starting. Both branches exercised on a scratch database migrated to
  099: with `Sales` beside `sales` the migration warned and installed the
  trigger (a third variant `SALES` refused); the clean dimension table got the
  indexes (`Region` beside `region` refused by `dimension_def_name_ci_uq`).

### Dimension names are unique only in their exact case

- **Noticed:** 2026-09-28, while making metric names case-insensitive (migration
  `100_metric_name_case.sql`) after the formula catalogue run.
- **What:** `dimension_def_with_rev_uq` / `dimension_def_null_rev_uq` (migration
  027) are unique on the exact name, but formulas match dimension names
  regardless of case (`metricformula.revisionDims.lookup`, the evaluator's
  `Dim.Current`). A revision can hold `Region` beside `region`; a formula naming
  either then resolves to whichever the lookup meets first.
- **Why it matters:** the same ambiguity migration 100 removed for metrics: a
  formula can silently read the other dimension.
- **How to check:** `SELECT model_id, revision_id, lower(name), count(*) FROM
  model.dimension_def GROUP BY 1, 2, 3 HAVING count(*) > 1;` and try creating
  `Region` beside `region` in the console (it is accepted today).
- **What closes it:** the migration 100 pattern for `dimension_def` (a trigger
  raising 23505, which the writers already answer as `DIMENSION_NAME_TAKEN`).
- **Closed:** 2026-09-29 (uncommitted). Migration `101_dimension_name_case.sql`
  adds the migration 100 pattern to `dimension_def` (a trigger raising 23505,
  answered as `DIMENSION_NAME_TAKEN`, whose message now says names are compared
  without regard to case). Proved by `TestDimensionNamesIgnoreCase` and live by
  `cmd/verify-formula-catalogue` (`Region` beside `region` → 409).

### The case-insensitive metric-name rule is a trigger, not an index

- **Noticed:** 2026-09-28, with migration `100_metric_name_case.sql`.
- **What:** metric names are unique regardless of case through a `BEFORE INSERT
  OR UPDATE` trigger, chosen so a database that already holds a pair such as
  `Sales` beside `sales` keeps starting. Two consequences: two concurrent
  creates of `Sales` and `sales` can both pass the check (a trigger sees no
  uncommitted row), and a revision that held such a pair before the migration
  cannot be duplicated, exported/imported or copied by the AI Developer until
  one of the two is renamed (the copy's insert trips the trigger).
- **Why it matters:** the first is a narrow race; the second surfaces as a
  refused copy naming `METRIC_NAME_TAKEN`, which the developer can resolve.
- **How to check:** `SELECT model_id, revision_id, lower(name), count(*) FROM
  model.metric_def GROUP BY 1, 2, 3 HAVING count(*) > 1;` on each deployment.
- **What closes it:** once no deployment returns rows, a unique index on
  `(model_id, revision_id, lower(name))` (and the revision-less variant) in
  place of the trigger.
- **Closed (the race):** 2026-09-29 (uncommitted). Both name triggers
  (migrations 100 and 101) take a transaction-scoped advisory lock on (model,
  revision, lower(name)) before checking, so a second concurrent writer waits
  for the first and is refused. `TestNameCheckSerialisesConcurrentWriters`
  proves it, and fails with the locks removed (both writers succeeded). The
  triggers have since become unique indexes wherever no legacy pair exists
  (closed entry "Name uniqueness regardless of case is enforced by triggers");
  the locked trigger remains only as the fallback for a database holding one.

### Numbers in formulas compare exactly, not on 15 digits as Excel does

- **Noticed:** 2026-09-28, by the formula catalogue
  (`internal/formula/catalogue_test.go`).
- **What:** `0.1 + 0.2 = 0.3` is FALSE: `compareValues` and the criteria of the
  `SUMIFS` family compare binary floats exactly. Excel compares on 15
  significant digits and answers TRUE. Rounding and number-to-text already use
  15 digits (fixed in the same change); comparisons were left alone because
  changing them changes which branch every existing `IF` takes near a boundary.
- **Why it matters:** a developer porting a spreadsheet can get a different
  branch of an `IF` on computed decimals. Documented in
  `FORMULA_CALCULATION_INSTRUCTIONS.md` ("Scalar semantics") and the formulas
  manual, with the advice to compare `ROUND(x, n)`.
- **How to check:** `go test ./internal/formula/ -run TestCatalogueScalar -v`
  (the case `ROUND(0.1 + 0.2, 2) = 0.3` and the manual's note).
- **What closes it:** a decision to compare on 15 significant digits in
  `compareValues` and `criterion.matches`, with the catalogue cases moved.
- **Closed:** 2026-09-29 (uncommitted), by decision to follow Excel.
  `compareValues` (`=`, `<>`, orderings, `SWITCH`) and the SUMIFS family's
  criteria (`criterion.matches`, and `numberText` for criteria and member codes)
  compare numbers on their 15 significant digits: `0.1 + 0.2 = 0.3` is TRUE,
  `1.00000000000001 = 1` still FALSE, and a truthiness test of a tiny
  difference (`IF(0.1 + 0.2 - 0.3, …)`) is unchanged, as in Excel. Catalogue
  cases in `internal/formula/catalogue_test.go`; live `cmp_15` and `crit_15` in
  `cmd/verify-formula-catalogue`.

### Background recalculations have no recover

- **Noticed:** 2026-09-28: `MID(text, start, negative)` panicked and
  `SUBSTITUTE(text, "", new, n)` never returned; both fixed, and every formula
  evaluation is now behind `EvalContext.safeEval`, which turns a panic into the
  cell's `#VALUE!`.
- **What:** the recalculations the gateway starts with `go h.recalc…` and
  `go func() { sched.RecalcAffected(…) }()` (`internal/gateway/handler.go`) have
  no `recover`. A panic anywhere else on that path — the scheduler, rollup, the
  store — still stops the whole gateway process, for every tenant, and a hang
  holds a goroutine forever (there is no per-recalculation deadline).
- **Why it matters:** before the fix, one developer saving
  `=LEN(MID("abc", 2, -1))` would have stopped the shared gateway.
- **How to check:** `grep -n 'go h.recalc\|go func() { _ = sched' internal/gateway/handler.go`.
- **What closes it:** a wrapper for these goroutines that recovers, logs and
  marks the recalculation failed, and a deadline on the recalculation context.
- **Closed:** 2026-09-29 (uncommitted). The scheduler recovers at its entry
  points (`RecalcAffected`, `RecalcSpecific`, `RecalcDimensionDependents`,
  `recoverAsError`) and per metric and per recurrence (`guarded`: the metric is
  marked failed and the pass goes on), which covers the gateway, the
  calculation service's RPCs and the `facts.committed` consumer. The gateway's
  recalculation helpers and its two inline launches run under
  `h.backgroundRecalc`, which recovers around the scheduler and bounds the run
  with `calculation.RecalcTimeout` (30 minutes), as do the calculation
  service's RPC launches. Tests: `TestRecalcEntryPointsRecover` (a nil store's
  real nil dereference comes back as an error), `TestGuardedTurnsPanicIntoError`,
  `TestBackgroundRecalcSurvivesPanic`. A CPU-bound loop inside a formula still
  ignores the deadline (the evaluator does not check the context); the known
  ones are fixed.

### The Add metric form checks only `{name}` references as you type

- **Noticed:** 2026-09-28, while documenting the Metrics screen for the formulas
  manual.
- **What:** the row editor lists every unknown name under the formula (from
  `/api/formula/refs`), but the Add metric form checks only `{name}` references
  (`MetricsTab.tsx`, `formula.match(/\{([^}]+)\}/g)`), and matches them in exact
  case. A bare name typo is reported only by the server when the metric is
  added. The form's hint also still suggests `{metric_name}` references,
  although plain names are the documented style.
- **Why it matters:** minor: the save still refuses the formula with a clear
  message; the add form is just less helpful than the row editor.
- **How to check:** in the console, add a Calc metric `=revenu * 2`; no hint
  appears until **Add metric**.
- **What closes it:** the add form using the row editor's `/api/formula/refs`
  hint.
- **Closed:** 2026-09-29 (uncommitted). The Add metric form uses the row
  editor's check (`useUnknownFormulaNames` in `MetricsTab.tsx`: the server's
  parser, names compared regardless of case, a hint that never blocks **Add
  metric**), and suggests plain names (`e.g. revenue - cost`). Driven in the
  running console against a live gateway: `revenu * 2 + region.factor + SALES`
  lists `revenu`; `MOVINGSUM(sales, -2, 0, AVERAGE) + LAG(sales, 1, 0, STRICT)`
  and `{sales} - Cost` list nothing.

### The AI Developer cannot delete a dimension member

- **Noticed:** 2026-09-28, looking for every path that deletes members.
- **What:** the only member delete is the developer's
  `DELETE /api/developer/dimensions/{dimId}/members/{memberId}`. The AI
  assistant has `add_dimension_member` and `update_dimension_member` but no
  delete tool; CSV and connector imports only upsert members; model import
  creates a new model; dimension and revision deletes remove members with
  their parent (the dimension delete is guarded by `DIMENSION_IN_USE`).
- **Why it matters:** AI Developer parity (standing rule 2): a model the
  developer can shape by removing a member cannot be shaped that way through
  the assistant.
- **How to check:** the write tool list in `internal/aiassistant/tools.go`.
- **What closes it:** a `delete_dimension_member` tool that calls
  `metricformula.CheckMemberNotInUse` before deleting, as
  `delete_dimension_property` calls `CheckPropertyNotInUse`.
- **Closed:** 2026-09-28 (uncommitted — the AI Developer parity audit). The
  assistant has `delete_dimension_member` (refused with `MEMBER_IN_USE`
  through `metricformula.CheckMemberNotInUse`) and the rest of the
  developer's change-and-remove actions. The member delete itself, with its
  move of input values to history and the time re-index, moved from the
  gateway to `internal/modeledit`, which both doors call. Covered by
  `TestDeleteDimensionAndMember` (`internal/aiassistant`).

### The grid averages a parent's children counting a missing value as 0

- **Noticed:** 2026-09-28, final review, while making chart-data leave out
  leaves with no value. Seen in the code, not reproduced in a browser.
- **What:** `resolveCell` in `web/src/consoles/business/PlanningGrid.tsx`
  combines a parent row's children with `childVals.push(v ?? 0)`, so an
  `agg_rule` average counts a child with no value as 0. The scheduler's totals
  and chart-data's points for a formula reading member metadata leave such a
  child out (a blank `region.factor`), and `rollup.Resolve` also counts a
  missing input leaf as 0.
- **Why it matters:** an EMEA row of `wavg = region.factor` over UK 2, DE 3 and
  FR (no factor) could show 1.67 in the grid and 2.5 in a chart.
- **How to check:** the fixture of `TestPropertyMetricBlankLeavesAndParents`
  (`internal/gateway`) on a grid whose rows show the region hierarchy.
- **What closes it:** one rule for "no value" in an average, applied by the
  grid client, `rollup.combineAgg` and the scheduler alike.
- **Server side:** closed 2026-09-28 (uncommitted, the flat-parent change):
  `rollup.Resolve` leaves a missing leaf out of an average. The grid client
  (`resolveCell`) is what remains.
- **Closed:** 2026-09-28 (uncommitted — the flat-parent change): the grid
  client reduces a parent's leaves once (`flatValue` in `PlanningGrid.tsx`),
  leaving a leaf with no value out of an average. Tests: the Playwright cases
  "a parent two levels up averages and counts its leaves flat" and "a
  calculated leaf is its own value; count skips zeros; a pure ratio is the
  server's" (`web/e2e/business-console-grid-smoke.spec.ts`).

### The grid's parent rows differ from chart-data for member-metadata metrics

- **Noticed:** 2026-09-28, final-review verification of the chart leaf-mean
  fix. Reproduced with a throwaway gateway test plus the extracted
  `resolveCell` source run in node. Not seen in a browser.
- **What:** chart-data, the scoped `/api/grid` total and the scheduler's slice
  rows now combine a member-metadata metric (for example `region.factor`) once
  over the leaves under a point, leaving out leaves with no value. The
  business grid's parent rows are still built in the browser by `resolveCell`
  in `web/src/consoles/business/PlanningGrid.tsx`, one level at a time, from
  the leaf cells the server returns. The server never returns parent-row
  cells for these metrics, even for a read scoped to World and one month.
  This extends "The grid averages a parent's children counting a missing
  value as 0" above in two ways:
  - an `agg_rule` average is a mean of means, and it counts a child with no
    value as 0;
  - an `agg_rule` count is the number of children whose value is not 0
    (`childVals.filter(v => v !== 0).length`), not the number of leaves that
    have a value.
- **Why it matters:** fixture Global > {World > {EMEA > {UK 2, DE 3, FR with no
  factor}, US 4}, APAC > {JP 10}}, one month. The chart, the scoped grid total
  and the scheduler give an average of EMEA 2.5, World 3, Global 4.75, and a
  count of 2, 3, 4. The grid's rows show an average of 1.67, 2.83, 6.42, and a
  count of 2, 2, 2. Sum agrees. Before the chart fix the chart's World bar
  (3.25) happened to match the grid's World row. Now it does not, and the
  harness F2 comment ("the live chart showed 3.25 while the grid showed 3")
  holds only for the grid's total, not for its World row.
- **How to check:** build the fixture above with `wavg = region.factor`
  (average) and `wcnt` (count) on a [region, period] grid. Open it in the
  business console and compare the EMEA, World and Global rows with
  `/api/dashboard-widgets/{id}/chart-data` of a chart by region.
- **What closes it:** needs a product decision, because the same client code
  combines input and served metrics too. The decision is one rule for an
  aggregate above one level: the leaf mean leaving out leaves with no value,
  which the scheduler and chart-data already use for member-metadata metrics;
  or the level-by-level rule. Then either the grid reads the server's value
  for a parent row (served two-dimension rollup rows, or scoped reads), or
  `resolveCell` reduces the leaves under the row once, as
  `rollup.ResolveTimeFlat` does, with count meaning the number of leaves that
  have a value. See also "Served and input metrics still average a two-level
  parent as a mean of means".
- **Server side:** decided 2026-09-28 (leaf mean everywhere) and done in the
  flat-parent change: every server reader combines `average` and `count`
  flat, including input and served metrics. `resolveCell` is what remains;
  once it reduces flat, this entry closes.
- **Closed:** 2026-09-28 (uncommitted — the flat-parent change): the grid
  client reduces every sum/average/count parent and the Total row flat over
  the leaf cells (`flatValue`), per leaf period and then by `time_summary`.
  A leaf is its own cell (a calculated count leaf had rendered as 1, a
  "count" of its one value), count is the number of leaves whose value is
  not 0 as in `rollup.CombineAgg`, and a metric the server flags
  `aggregate_evaluated` (formula, rate, a pure-ratio average) takes its
  parents from the server's rows. Tests: the Playwright cases named in the
  entry above.

### A ratio-average metric on a time dimension gets no total row

- **Noticed:** 2026-09-28, reading `executePartition`; not run.
- **What:** a metric with agg_rule `average` whose formula is not
  dimension-conditional collapses to one evaluation at `{}`. On a
  time-dimensioned grid the total then goes through `summarizeOverTime`,
  which finds no period in that one `{}` result and writes no total. The
  gateway's scoped read still combines the single value into a total.
- **Why it matters:** the unscoped grid shows no total ("—") where a scoped
  read shows a number.
- **How to check:** `revenue / units` with agg_rule `average` on
  `[region, month]`; write facts; read the metric's `'{}'` row.
- **What closes it:** skipping the time reduction for the collapsed case in
  the scheduler (the value already is the total), or deciding the metric has
  no total and matching that in the scoped read.
- **Closed:** 2026-09-28 (uncommitted — the flat-parent change):
  `executePartition` skips `summarizeOverTime` for the collapsed case, whose
  one `evalOne({})` result already is the total (the scoped read's collapsed
  `evalCombo` gives the same number). A developer's World-scoped total and a
  restricted viewer's now both exist. Test:
  `TestPureRatioAverageReadsTheSchedulersAggregate` (`internal/gateway`,
  `rav`).

### The OpenAPI document does not name INVALID_PARENT_DIMENSION

- **Noticed:** 2026-09-28, adding the same-revision parent check.
- **What:** `POST` and `PATCH /api/developer/dimensions` now refuse a bad
  `parent_dimension_id` with a 400 starting `INVALID_PARENT_DIMENSION`
  (another revision or model, itself, a cycle, a time dimension). The 400
  was already answered with a plain message; `api/openapi.yaml` describes the
  400 of these routes by `INVALID_GROUPING` only.
- **Why it matters:** a client reading the spec does not know the code.
- **How to check:** `grep INVALID_PARENT_DIMENSION api/openapi.yaml`.
- **What closes it:** naming the code in the two 400 descriptions and
  regenerating `internal/gateway/oas` (left out of this change because the
  spec was being edited in parallel).
- **Closed:** 2026-09-28 (uncommitted — this change). The 400 descriptions of
  `POST /api/developer/dimensions` and `PATCH /api/developer/dimensions/{dimId}`
  name `INVALID_PARENT_DIMENSION` and its four causes; `internal/gateway/oas`
  was regenerated with ogen v1.20.3 (`--target internal/gateway/oas --package
  oas --clean`) and `go build ./...` and `TestRouteSpecParity` pass. In the
  same pass, the PATCH's 409 `DIMENSION_IN_USE` (clearing or replacing a
  grouping's source) had its context text *before* the code, against the
  Error schema's "starts with its code" contract; `PlanGroupingPatch` now keeps
  the code first, and `TestPropertyGroupingDimensionOverHTTP` and
  `TestUpdateDimension` assert the prefix.

### The AI Developer cannot change an existing dimension

- **Noticed:** 2026-09-28, while giving `create_dimension` property groupings.
- **What:** the developer's `PATCH /api/developer/dimensions/{dimId}` renames a
  dimension, changes its rollup rule, sets or detaches its parent dimension, and
  sets, changes, re-derives or clears its property grouping. The AI Developer
  has no `update_dimension` tool: it can set a parent or a grouping only when it
  creates the dimension, and cannot re-derive a grouping's members after new
  property values appear (it can add them one by one with
  `add_dimension_member`).
- **Why it matters:** standing rule 2 — a model the AI builds cannot have a
  dimension's structure corrected by the AI.
- **How to check:** the `Execute` switch in
  `internal/aiassistant/write_executor.go` has no dimension update case.
- **What closes it:** a generic `update_dimension` tool reusing the PATCH's
  rules (`metricformula.ValidateGrouping`, the parent cycle check, the
  `DIMENSION_IN_USE` refusal on clearing a grouping's source).
- **Closed:** 2026-09-28 (uncommitted — this change). The AI Developer has an
  `update_dimension` write tool, the twin of the PATCH: a partial update of
  name, rollup rule, tags, parent dimension (by id or name; null detaches) and
  property grouping (set, change, clear, `derive_members`), through
  propose -> confirm and `ai_proposal.confirmed`. The PATCH's rules moved into
  `metricformula` (`ValidateParentDimension`, `PlanGroupingPatch`), and both
  paths call them, so they cannot drift. Covered by `TestUpdateDimension`
  (`internal/aiassistant`), `TestAIUpdateDimensionIsAudited` and
  `TestAIDeveloperChangesAGrouping` (`internal/gateway`, which promotes and
  reads a grid by the regrouped dimension).

### A dimension's parent dimension is not checked for the same revision

- **Noticed:** 2026-09-28, while adding the same-revision rule for property
  groupings; not run.
- **What:** the developer dimension create and PATCH check only that
  `parent_dimension_id` belongs to the same model, and the AI's
  `create_dimension` resolves `parent_dimension_name` by name across every
  revision of the model (`LIMIT`-less `QueryRow`, first row wins). A property
  grouping's source is refused unless it is in the same revision.
- **Why it matters:** a parent in another revision links two revisions'
  structures; a revision copy then remaps nothing for it, and rollup, which
  loads one revision's dimensions, silently finds no relation.
- **How to check:** create a dimension in revision B with
  `parent_dimension_id` of a dimension of revision A; it is accepted.
- **What closes it:** the same-revision check the grouping validator makes,
  and the AI resolving the parent within its working revision.
- **Closed:** 2026-09-28 (uncommitted — this change).
  `metricformula.ValidateParentDimension` refuses a parent of another revision
  (as well as another model, the dimension itself, a cycle or a time
  dimension) with `INVALID_PARENT_DIMENSION` (400). The developer POST and
  PATCH and the AI's `create_dimension` and `update_dimension` all call it,
  and the AI resolves `parent_dimension_name` within its working revision.
  Covered by `TestDimensionParentSameRevision` (`internal/gateway`) and
  `TestUpdateDimension` (`internal/aiassistant`).

### Served and input metrics still average a two-level parent as a mean of means

- **Noticed:** 2026-09-28, fixing the same shape for chart-data's
  member-metadata branch; seen in the code, not reproduced.
- **What:** `rollup.Resolve`/`ResolveTime` combine a same-dimension hierarchy
  level by level, so with `agg_rule` average (or count) a member two levels up
  is a combination of its children's combinations. Chart-data still uses
  that for input metrics and for served metrics (LOOKUP, `*IFS`, time
  functions) — only the member-metadata branch now reduces the leaves flat
  (`rollup.ResolveTimeFlat`). The scheduler's slice rows for a served metric
  are `CombineAgg` over its leaf rows, flat.
- **Why it matters:** a World bar of an average served metric could differ
  from the grid's World slice row when EMEA and AMER have different numbers
  of leaves with a value; for inputs the grid client (`resolveCell`) is also
  level by level, so the two agree with each other but not with a leaf mean.
- **How to check:** an average `SUMIFS`-based metric on the region fixture
  of `TestChartPropertyAverageTwoLevelsIsLeafMean`; compare the World point
  with the grid's World slice row.
- **What closes it:** a decision on one rule for an average above one level
  (leaf mean everywhere is the scheduler's), then `ResolveTimeFlat` in the
  served and input branches and the grid client.
- **Closed:** 2026-09-28 (uncommitted — this change), with the product
  decision "leaf mean everywhere": `rollup.Resolve`/`ResolveTime` combine
  `average` and `count` flat over the distinct leaves with a recorded value
  (`resolveFlat`, the traversal `ResolveTimeFlat` used), per leaf period and
  then by `time_summary` when a parent and an aggregate period are both
  pinned. Chart-data for input and served metrics, the scheduler's reads of
  a dependency and LOOKUP/`*IFS` resolution therefore agree with the
  scheduler's totals and slice rows. Two further server readers were not
  flat and are fixed with it: the grid's input totals summed an `average` or
  `count` input regardless of its rule (now `applyInputAggregation`), and
  chart-data evaluated a `sum`/`count` calculated metric once at the point's
  aggregated inputs (now per leaf, `leafWise` in `internal/query/chart.go`).
  Tests: `TestResolveAverageAndCountAreFlatOverLeaves`,
  `TestResolveTimeParentAndAggregatePeriodOrder` (`internal/rollup`) and
  `TestParentRowsCombineLeavesFlat` (`internal/gateway`: unscoped,
  totals-only and World-scoped totals equal, EMEA scope and chart-data at
  World, EMEA and the leaves).

### The OpenAPI document does not list the 409 of a dimension or property delete

- **Noticed:** 2026-09-28, final review, adding `DIMENSION_IN_USE`.
- **What:** `DELETE /api/developer/dimensions/{dimId}` (`DIMENSION_IN_USE`) and
  `DELETE /api/developer/dimensions/{dimId}/properties/{propId}`
  (`PROPERTY_IN_USE`) answer 409, but `api/openapi.yaml` lists only 200 for
  both.
- **Why it matters:** a client generated from the spec does not expect the
  refusal.
- **How to check:** the two `delete:` entries in `api/openapi.yaml`.
- **What closes it:** adding the 409 responses and regenerating
  `internal/gateway/oas`.
- **Closed:** 2026-09-28 (uncommitted — this change). `api/openapi.yaml` now
  lists every refusal these routes answer: 409 `DIMENSION_IN_USE` on the
  dimension DELETE and PATCH (grouping source), 409 `PROPERTY_IN_USE` on the
  property DELETE, 409 `MEMBER_IN_USE` on the member DELETE, 409
  `MEMBER_CODE_TAKEN` on the member POST and PATCH, 409 `DIMENSION_NAME_TAKEN`
  on the dimension POST and PATCH, 409 `METRIC_NAME_TAKEN` on the metric POST
  and PATCH; the 400s `INVALID_GROUPING` (dimension POST/PATCH),
  `INVALID_PROPERTY_NAME` / `INVALID_PROPERTY_TYPE` / `PROPERTY_NAME_TAKEN`
  (property POST/PATCH, which answer 400, not 409), the new 404s on member
  and property PATCH/DELETE, and a shared `FormulaRejected` 400 naming the
  formula codes on metric POST/PATCH, revision activate and grid membership.
  The `Error` schema says a refusal starts with its code. Also documented:
  `Metric.value` (null for a calculated metric when the caller has a hidden
  member), the 403 of `/api/developer/debug/calc` with `dim_members`, and
  500 on `/api/metrics`, `/api/dimensions` and
  `/api/business-admin/available`. `internal/gateway/oas` regenerated with
  ogen v1.20.3; `TestRouteSpecParity` passes.

### The OpenAPI document does not list MEMBER_IN_USE or the NAME_TAKEN 409s

- **Noticed:** 2026-09-28, adding them.
- **What:** member delete (409 `MEMBER_IN_USE`), dimension create and rename
  (409 `DIMENSION_NAME_TAKEN`), and metric create and PATCH (409
  `METRIC_NAME_TAKEN`) are not in `api/openapi.yaml`, like the dimension and
  property delete 409s above.
- **Why it matters:** a client generated from the spec does not expect the
  refusals.
- **How to check:** the matching entries in `api/openapi.yaml`.
- **What closes it:** adding the responses and regenerating
  `internal/gateway/oas`, together with the entry above.
- **Closed:** 2026-09-28 (uncommitted — this change), with "The OpenAPI
  document does not list the 409 of a dimension or property delete" above.

### The OpenAPI document does not list MEMBER_CODE_TAKEN

- **Noticed:** 2026-09-28, final-review fix round, when `MEMBER_CODE_TAKEN`
  was added.
- **What:** the developer member POST and PATCH (standard and time
  dimensions) now answer a duplicate code with 409 `MEMBER_CODE_TAKEN`. The
  response is not in `api/openapi.yaml`, like `MEMBER_IN_USE` and the
  NAME_TAKEN codes ("The OpenAPI document does not list MEMBER_IN_USE or the
  NAME_TAKEN 409s").
- **Why it matters:** a client generated from the spec does not expect the
  refusal.
- **How to check:** the member paths in `api/openapi.yaml`.
- **What closes it:** adding the response and regenerating
  `internal/gateway/oas`, together with the entry above.
- **Closed:** 2026-09-28 (uncommitted — this change), with "The OpenAPI
  document does not list the 409 of a dimension or property delete" above.

### A duplicate member code answers 500 with the SQL error

- **Noticed:** 2026-09-28, final review (a throwaway HTTP test). It was never
  recorded as open.
- **What:** `POST /api/developer/dimensions/{id}/members {"code":"EMEA"}`, when
  EMEA exists, and a member `PATCH` that recodes a member to a taken code,
  answered 500 with the text of `dimension_member_dimension_id_code_key`. The
  AI's `add_dimension_member` wrapped the same violation as a generic
  "insert dimension member" error.
- **Closed:** 2026-09-28 (uncommitted, final-review fix round).
  `metricformula.MemberCodeTaken` maps that constraint, and only that one: a
  time dimension's `time_index` and `period_start` uniqueness is left alone.
  It returns 409 `MEMBER_CODE_TAKEN` on the developer member POST and PATCH
  for standard and time dimensions, and on the AI's `add_dimension_member`
  for both kinds. The console's standard dimension card now shows the error
  on member add and edit; before, it dropped the failure silently. The AI's
  `update_dimension_member` cannot change a code. Tests:
  `TestMemberCodeTakenIsConflict` (`internal/gateway`),
  `TestAIAddDimensionMemberCodeTaken` (`internal/aiassistant`). Both fail
  without the mapping. The harness F6 block checks the POST and PATCH.

### MEMBER_IN_USE missed a SWITCH on the dimension

- **Noticed:** 2026-09-28, final review. `SWITCH(region, "APAC", 1, 0) * revenue`
  let APAC be deleted (200), while `IF(region = "LATAM", ...)` refused LATAM.
- **Closed:** 2026-09-28 (uncommitted, final-review fix round).
  `comparesMemberCode` treats each literal match value of a `SWITCH` on the
  bare dimension or `PARENT(dim)` as naming the member, but not the trailing
  default. Tests: new cases in `TestFormulaNamesMember` (four fail without the
  fix) and `TestMemberDeleteRefusedWhileSwitchNamesIt` (`internal/gateway`).

### A developer cannot create a property-grouping dimension over HTTP

- **Noticed:** 2026-09-28, while adding a live check for the fix to recalculate
  along property groupings (final review F5).
- **What it was:** the developer dimension create/PATCH and the AI's
  `create_dimension` accepted neither `source_dimension_id` nor
  `source_property`; such a dimension came into being only by model import or
  revision copy, and the harness could check F5 only through a
  `parent_dimension_id` relation.
- **Closed:** 2026-09-28 (uncommitted, this change): `POST`/`PATCH
  /api/developer/dimensions` and the AI's `create_dimension` take
  `source_dimension_id`, `source_property` and `derive_members`, validated by
  one `metricformula.ValidateGrouping` (`INVALID_GROUPING`); the developer
  console's New dimension form has **Group members of / By property** and the
  card **Derive members**. A property a grouping uses cannot be deleted, and
  metrics placed on a grid with a related dimension now recalculate on a member
  edit. The harness's F5 builds a grouping over HTTP
  (`TestPropertyGroupingDimensionOverHTTP`, `TestCreatePropertyGroupingDimension`).

### A duplicate dimension name in a revision answers 500 with the SQL error

- **Noticed:** 2026-09-28, live, in the harness's first regressions run.
- **What:** `POST /api/developer/dimensions {"name":"region","agg_rule":"sum",
  "revision_id":<a revision that already has region>}` answers 500 `insert
  dimension: ERROR: duplicate key value violates unique constraint
  "dimension_def_with_rev_uq" (SQLSTATE 23505)`. (A revision created without
  `source_revision_id` is a copy of the active one, so it already had
  `region`.)
- **Why it matters:** a normal user mistake gets a server error that shows the
  schema, where it should get a 4xx with a code, as properties do
  (`PROPERTY_NAME_TAKEN`).
- **How to check:** the request above.
- **What closes it:** a 409/400 with a `DIMENSION_NAME_TAKEN`-style code.
- **Closed:** 2026-09-28 (uncommitted, the `MEMBER_IN_USE` change): the
  developer's dimension create and rename answer 409
  `DIMENSION_NAME_TAKEN`, and metric create and rename 409
  `METRIC_NAME_TAKEN` (`metricformula.DimensionNameTaken` /
  `MetricNameTaken` map the unique violation); the AI's `create_dimension`,
  `create_metric` and `update_metric` return the same codes. The same run
  found `PATCH /api/developer/metrics/{id} {"formula": ...}` answering 500
  `metric_def_with_rev_uq`: the handler wrote the omitted name as "" (and
  reset every other setting), so a revision's second such PATCH collided.
  PATCH and the AI's `update_metric` are now partial updates. Tests:
  `TestDefinitionNameTakenAndPartialMetricPatch` (`internal/gateway`),
  `TestAINameTakenAndPartialUpdateMetric` (`internal/aiassistant`); the
  harness's F6 sends a formula-only PATCH.

### Chart-data averages a dim.property metric two levels up as a mean of means

- **Noticed:** 2026-09-28, live, adding the final review's regressions to
  `cmd/verify-formula-lookups` (check "F2 chart-data by geo @2026-02: wavg/wsum
  World combines its leaves").
- **What:** `wavg = geo.factor` (`agg_rule` average) over DE 3, UK 2, FR (no
  factor) under EMEA and US 4 under AMER. `POST
  /api/dashboard-widgets/{id}/chart-data {"context":{"<month>":"2026-02"}}`
  answers EMEA 2.5, AMER 4 and World **3.25**, the mean of EMEA's and AMER's
  means. The scheduler's persisted total and the grid total are 36, which is
  12 months × **3**, the mean of the leaves. `debug/calc` with
  `dim_members={"<geo>":"World"}` also answers 36. The chart's leaf branch
  (`internal/query/chart.go`, the dim.property path) hands its leaf evaluator
  to `rollup.ResolveTime`, which combines level by level.
- **Why it matters:** a dashboard's World bar disagrees with the grid's total
  and with the scheduler whenever the parents have different numbers of
  leaves with a value.
- **How to check:** `go run ./cmd/verify-formula-lookups`. That one check
  fails. Every other check passes.
- **What closes it:** chart-data averaging the leaves under the point
  directly, the way the scheduler combines them. The check then passes.
- **Closed:** 2026-09-28 (uncommitted, this change): the chart's leaf branch
  calls the new `rollup.ResolveTimeFlat`, which gathers the distinct leaves
  under the point and combines their values once by `agg_rule` (per leaf
  period, the periods then by `time_summary`), leaves with no value left
  out: the scheduler's `CombineAgg`/`summarizeOverTime`. Tests:
  `TestResolveTimeFlat*` (`internal/rollup/flat_test.go`) and
  `TestChartPropertyAverageTwoLevelsIsLeafMean` (`internal/gateway`, World
  3 not 3.25, and 18 over all periods, equal to the grid total).

### The scalar recalculation clears a metric's rows before it writes the new ones

- **Noticed:** 2026-09-28, live. One run in about seven of
  `cmd/verify-formula-lookups` failed "viewer cells: fx_region" and "viewer
  total: fx_region", with every DE/UK cell missing and none withheld. Every
  later read of the same cells passed.
- **What:** `internal/calculation/scheduler.go` runs
  `ClearPerComboResults` and then `WriteCalcResults` as two separate
  statements, outside a transaction (around line 563). A grid read between
  them sees none of the metric's cells. The time-series path already uses
  `ClearPerComboResultsTx` in a transaction (`timeseries.go` ~849). In the
  harness, the UK `weight` edit starts a recalculation of every metric that
  names `region`, including `fx_region`, and the viewer's read can land inside
  that recalculation.
- **Why it matters:** during any recalculation, users briefly see blank cells
  and totals. Before the change they saw the last complete values.
- **How to check:** repeat the harness. The failure is intermittent. For a
  deterministic check, read the grid in a loop while a member property edit
  recalculates a large metric.
- **What closes it:** clearing and writing in one transaction, as the
  time-series path does.
- **Closed:** 2026-09-28 (uncommitted, this change): the scalar path and the
  non-recurrence time-series path replace a metric's result set (its `'{}'`
  total or the clear of a stale one, the clear of the per-combo rows, the new
  rows) in one transaction (`Store.InTx`); the time-series path had in fact
  also written outside a transaction — only the recurrence path used one.
  The non-transactional `WriteCalcResult(s)`, `ClearPerComboResults` and
  `ClearAggregateResult` are gone, so no caller can reintroduce the gap; the
  remaining pool write, `ClearAllResults`, is a single statement. A failed
  write now rolls back the whole set, total included, keeping the last good
  rows. Test: `TestRecalcNeverExposesAnEmptyResultSet`
  (`internal/calculation`): two readers count a scalar and a time-series
  metric's rows through 40 recalculations; with the clear split into its own
  transaction it saw 90 of 2951 reads empty, with the fix none.

### A rename joins identities for good, so access rules can over-restrict

- **Noticed:** 2026-09-28, closing the old-revision access-rule gap.
- **What it was:** access rules resolved by name, following a permanent log
  of renames (`model.definition_rename`), so names joined by any rename — in
  any revision, even a draft never activated — counted as one identity for
  good, and a rule on one could restrict the other.
- **Closed:** 2026-09-28 (uncommitted, this change), before that design was
  released: migration 099 was rewritten to lineage ids. Dimensions, members
  and metrics carry a `lineage_id` shared only by their copies (revision
  duplication, the AI `create_revision`, model export/import), and a rule
  resolves by the lineage of the row it was written on
  (`writeguard.RulesForRevision`). There is no rename log and no name
  matching, so a rename cannot join two identities. A member or metric
  deleted and re-added is a new lineage, unrestricted until an admin sets a
  rule on it.

### The AI Developer cannot rename, retype or delete a property declaration

- **Noticed:** 2026-09-28, while adding `add_dimension_property`.
- **What:** the AI Developer can declare a typed property
  (`add_dimension_property`) and set member values, but has no tool to rename,
  retype or delete a declaration. A developer can, through
  `PATCH`/`DELETE /api/developer/dimensions/{id}/properties/{propId}`. The
  rename path also moves member values (`metricformula.RenamePropertyValues`)
  and triggers a recalculation.
- **Why it matters:** standing rule 2. A model the AI builds can't have a
  mistyped or misnamed property corrected by the AI itself.
- **How to check:** look at the `Execute` switch in
  `internal/aiassistant/write_executor.go`. There is no update or delete
  property case.
- **What closes it:** generic `update_dimension_property` and
  `delete_dimension_property` tools that reuse
  `metricformula.ValidatePropertyDeclaration` and `RenamePropertyValues`.
  This needs approval under standing rule 3.
- **Closed:** Closed 2026-09-28 (uncommitted, this change): `update_dimension_property` and `delete_dimension_property` mirror the developer PATCH/DELETE — same validator, value migration on rename, draft- and model-scoped, audited.

### A bare dimension name reads #NAME? where dim.property reads blank

- **Noticed:** 2026-09-28, documentation review.
- **What:** at a total, and at a rollup or slice row that leaves a
  dimension unpinned, a bare dimension name (`region = "EMEA"`) is unbound
  and evaluates to `#NAME?`. `region.segment` and `PARENT(region)` read blank
  in the same place.
- **Why it matters:** an `agg_rule = formula` metric that tests the current
  member fails its total-level evaluation, while the same test written with a
  property works. This existed before the dimensional references; they only
  make the inconsistency visible.
- **How to check:** create `=IF(region = "EMEA", revenue, 0)` with
  `agg_rule = formula` and recalculate. The metric reports the total-level
  failure.
- **What closes it:** a decision to bind an unpinned dimension to blank, as
  `dim.property` does. That changes how existing formulas behave at totals.
- **Closed:** Closed 2026-09-28 (uncommitted, this change): a bare dimension name that is not pinned now reads blank in every evaluator (`formula.EvalContext.unboundDimension`); an `agg_rule = formula` total that tests the member computes instead of failing.

### DYNAMIC_TIME_OFFSET_UNSUPPORTED is now narrower than its name

- **Noticed:** 2026-09-28.
- **What:** dynamic `LAG`/`LEAD`/`OFFSET` offsets are supported now. The
  code fires only for a decimal literal offset, or a non-literal `MOVINGSUM`
  window.
- **Why it matters:** the name misleads whoever reads the message or handles
  the code. Renaming it would change an API-visible code.
- **How to check:** `grep -n CodeDynamicTimeOffset internal/formula`.
- **What closes it:** a deliberate rename, noted for API clients, or leaving
  it as is and documenting what it covers.
- **Closed:** Closed 2026-09-28 (uncommitted, this change): replaced by `TIME_OFFSET_NOT_INTEGER` (decimal literal at save, non-whole dynamic offset at run time) and `MOVING_WINDOW_NOT_LITERAL`; the time-series spec §9 records the replacement for API clients.

### Production's Go standard library was current only by accident

- **Noticed and closed:** 2026-09-27, `0a74995`.
- `govulncheck` found standard-library vulnerabilities (net/http, crypto/tls,
  html/template, …) fixed in Go 1.26.6. The service images build
  `FROM golang:1.26-bookworm`, a floating tag that was already 1.26.8, but
  `go.mod` said 1.26.3, which is what CI's tests and local builds used.
  `go.mod` now requires 1.26.8. The official golang image sets
  `GOTOOLCHAIN=local`, so an older base image now fails the build instead of
  shipping an old standard library.

### Scoped reads leaked hidden members through served values

- **Noticed and closed:** 2026-09-28, in the dimensional-references change
  (contracts C6 and C7; not yet committed when written).
- The scoped grid read served time-series metrics with a window check on the
  time dimension only, built its period order with the aggregate periods
  interleaved (a hidden January leaked into a served February) and summed
  the persisted Q1/H1 rows back into Q1 (120 for 60). Chart-data served them
  with no check at all and turned a failed dependency into 0. `GET
  /api/metrics` returned the whole-model calculated totals to a caller with
  hidden members, and `grid()` treated a caller whose access rules failed to
  load as unrestricted. All served values now go through the per-metric
  read sets of `internal/readset` on both paths, the metric list returns
  null for calculated values to a restricted caller, and the grid fails with
  500. Tests: `restricted_reads_test.go`,
  `TestScopedSeriesSuppressesHiddenWindow`, `internal/readset`.
- In the same change, before it was committed, a verification pass found
  three more defects, and they were fixed.
  - A member-local LOOKUP member or `*IFS` criterion inside a time window
    (`PREVIOUS(LOOKUP(revenue, period, period))`) was checked only at the
    cell's own period. The scheduler evaluates it at the shifted period, so
    a hidden January was served at February. The read set now evaluates it
    at every period the window reaches.
  - Recurrences were withheld in both time directions.
  - Chart-data plotted a `formula`/`rate` served metric at an aggregate
    point as the mean of its children, not the persisted rollup row.
  - Tests: `TestRestrictedViewerWindowedMemberLocal`,
    `TestRestrictedViewerRecurrenceReadsOnlyThePast`,
    `TestChartServesFormulaRuleRollupRow`, `TestWindowedMemberLocal`,
    `TestRecurrenceDirection`.
