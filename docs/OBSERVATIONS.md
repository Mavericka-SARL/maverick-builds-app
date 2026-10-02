# Observations

> **Classification:** Current — Things noticed about this codebase and its
> dependencies that are not fixed yet, with the evidence and what would close
> each one.

> **Last verified:** 2026-10-02

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

### The home page's dashboard illustration copies the console's widgets

- **Noticed:** 2026-10-01.
- **What:** the hero illustration on maverickbuilds.app (private deploy
  repository, `prod/site/index.html` and the `.mock` rules in `site.css`) draws
  a grid with a selector strip, a bar chart, a KPI and an automation-button
  widget in markup. Their structure and sizes were copied from a live render of
  the console's widgets (`web/src/ui/design-system.css` and the widget
  components); the colours are the site's own. Nothing links the two.
- **Why it matters:** the illustration is meant to show what the product's
  dashboards look like. A change to the widgets' anatomy — the grid's two-level
  header, the boxed editable cells, the CALC row, the KPI tile — leaves the home
  page showing something the product no longer draws.
- **How to check:** render a dashboard with a grid, chart and KPI and compare
  its layout against the hero illustration.
- **What closes it:** a screenshot comparison in the site's release steps, or
  replacing the illustration with an image rendered from a real dashboard.

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
  be shown a role it cannot then change ("not found in this workspace"). And
  a business role counts as a step's assignee only in its own workspace's
  applications and its tenant's tenant-level ones
  (`internal/workflow/assignee/assignee.go:40-46`), so a step the assistant
  assigns to another workspace's listed role is assigned to no one.
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

### A person who holds only tenant_admin cannot open the Tenant admin guide

- **Noticed:** 2026-09-29, adding the getting-started guides.
- **What:** sign-up now adds three guide models beside the tour (Developer,
  Business admin and Tenant admin guide), each a set of dashboard pages.
  Dashboards are read under Run or Business Admin, which only
  `business_user` and `business_admin` switch on
  (`web/src/router/sections.ts:33-34`). A person who holds only
  `tenant_admin` gets the Tenant admin group, which has no dashboard screen,
  so the guide written for them is out of their reach. The guide itself
  tells the reader to give such a person a business role as well.
- **Why it matters:** standing rule 2: the guide is not reachable by the
  role it is written for without a second role.
- **How to check:** invite a user with only `tenant_admin`, sign in as them,
  and look for a dashboard screen.
- **Decision:** accepted by the user on 2026-09-29: no separate dashboard
  screen for tenant admins for now. The guide is read by the sign-up owner,
  who holds more roles, and by anyone also given a business role.
- **What closes it:** only if that decision changes: a read-only dashboard
  view in the Tenant admin group, or the guide delivered some other way.

### Tenants that signed up before 2026-09-29 have no guide models

- **Noticed:** 2026-09-29, by decision.
- **What:** the three guides are imported only by sign-up
  (`internal/gateway/signup.go:231`, `starter.Packages()`). Nothing adds them
  to an existing tenant, and they exist only as Go code (`internal/starter`),
  not as a package file a tenant admin could import.
- **Why it matters:** a tenant created earlier has no in-product guide for
  any role.
- **How to check:** list the models of an application created before this
  change.
- **Decision:** accepted by the user on 2026-09-29: new sign-ups only.
- **What closes it:** only if that decision changes: a published export
  package of each guide, imported with the tenant admin's model import.

### Business users in no business role see every guide page

- **Noticed:** 2026-09-29, building the tour.
- **What:** a person who is a member of no business role sees every
  dashboard of the model they open; role membership is what narrows it
  (`businessDashboards`, `internal/gateway/handler.go:14131-14160`, the
  owner's decision of 2026-08-30). An invited business user who is put in
  no role therefore sees the tour's and the guides' pages when they pick
  those models in the model switcher.
- **Why it matters:** the guides are written for the tenant's builders and
  admins; business users may be shown pages about roles and access rules.
- **How to check:** invite a business user, put them in no role, and open
  the Business admin guide model as them.
- **What closes it:** a decision whether business users should see the
  guides; if not, sign-up granting the guide dashboards to named roles, or
  model-access restrictions.

### A member added or re-parented after a reorder lists after the last subtree

- **Noticed:** 2026-09-29, reviewing the member reorder.
- **What:** a reorder renumbers the whole dimension 1..N in depth-first
  tree order, so every flat reader (`ORDER BY time_index, sort_order, code`:
  grid axes, `GET /api/dimensions`, chart axes, selectors) lists each member
  right after its parent. Member create still appends `MAX(sort_order)+1`
  (the developer member add in `internal/gateway/handler.go`,
  `internal/integration/commit.go`, `internal/metricformula/grouping.go`,
  the AI's `write_executor.go`), and a re-parent keeps the member's number.
  So a child added under the first top-level member afterwards lists after
  the last top-level member's subtree, until the next reorder of any level
  renumbers the dimension. Order among siblings stays right: Build ›
  Dimensions groups by parent.
- **Why it matters:** a grid or picker that shows members flat shows the new
  member away from its parent until someone reorders.
- **How to check:** reorder a level, add a child under the first top-level
  member, then `GET /api/dimensions`: the child is listed last.
- **What closes it:** member create and re-parent placing the member after
  its last sibling's subtree, through one shared `internal/modeledit` helper
  used by every create path (renumbering the dimension as the reorder does).

### The gRPC member create writes sort_order 0 and the old parent column

- **Noticed:** 2026-09-29, when grid axes and selectors started following
  `sort_order`; split out when the member reorder closed.
- **What:** `ModelService.CreateDimensionMember`
  (`internal/model/store.go:161`, `:198`) inserts without `sort_order`, so
  the member gets 0 and lists before every appended member; every other
  create path appends (`TestMemberCreatePathsAppend`). It also writes the
  hierarchy to `parent_id` (migration 004), and its reads return that
  column, while the gateway, the AI, revision copies and export read
  `parent_member_id` (migration 018): a parent set through gRPC is invisible
  everywhere else. Separately, the aggregate periods of a time dimension (no
  dates, so no `time_index`) keep `sort_order` 0 and list after the leaves
  in code order (FY26, H1, Q1, …): `modeledit.WriteTimeMember` and
  `InsertPeriods` (`internal/modeledit/modeledit.go:104`, `:143`) insert
  without `sort_order`, and `timedim.ValidateAndReindex`
  (`internal/timedim/timedim.go:486`) numbers only dated leaves. A reorder
  refuses time dimensions. Re-checked 2026-09-29: both still hold.
- **Why it matters:** anything creating members over gRPC gets them first
  in every list and without their parent; a time dimension's quarters,
  halves and years list in alphabetical order.
- **How to check:** `grep -n "INSERT INTO model.dimension_member"
  internal/model/store.go`: neither statement names `sort_order` or
  `parent_member_id`.
- **What closes it:** the gRPC create appending `MAX(sort_order)+1` and
  writing `parent_member_id`, with a test; or retiring the gRPC member create
  if nothing calls it. For aggregate periods, an order derived from their
  children's dates (for example each aggregate after its last leaf, or by
  its first leaf), set where the time dimension is re-indexed.

### Build groups a child dimension's members by parent; grids list them by sort_order

- **Noticed:** 2026-09-29, reviewing the member reorder; re-checked after
  the reorder began keeping each group's slots.
- **What:** for a dimension whose members hang off another dimension's
  members (`parent_dimension_id`), Build › Dimensions lists them grouped by
  that parent, in the parent dimension's order
  (`web/src/consoles/developer/DimensionsTab.tsx:524-535`), and its arrows
  move a member within its group. Every other reader lists the members flat
  by `sort_order`. The reorder counts such members as top-level and, when
  one group is reordered, gives its members the slots the group held
  (`internal/modeledit/reorder.go:218-230`), so groups that were
  interleaved stay interleaved. Members added in the order A1 (parent P),
  B1 (parent Q), A2 (parent P) show in Build as P: A1, A2; Q: B1, and in a
  grid, selector or chart as A1, B1, A2, before and after any reorder.
- **Why it matters:** the order a developer arranges in Build is not the
  order business users see in grids.
- **How to check:** a child dimension with members added across two
  parents in alternating order; compare Build with `GET /api/dimensions`.
- **What closes it:** a decision on which order is right. Either the flat
  readers order such a dimension by its parent's order first, or the
  renumbering places each group together in the parent dimension's order,
  as Build shows it.

### Connector names are not unique, and revision copies match connectors by name

- **Noticed:** 2026-09-29, proved with a probe test (since deleted).
- **What:** `model.integration_def` has no unique constraint on model,
  revision and name, and no create or rename path refuses a duplicate name.
  Revision copies map connectors to their copies by name. The dashboard-widget
  remap for `integration_button` (`internal/gateway/handler.go:4701-4703`,
  `internal/aiassistant/write_executor.go:2611-2613`) then fails the whole
  copy with "more than one row returned by a subquery used as an
  expression"; the automation-rule scope keeps the source reference instead
  (`HAVING count(*) = 1`, `handler.go:4644`). Model export also resolves by
  name and skips a name that matches more than one row, so the import drops
  that reference.
- **Why it matters:** a developer who gives two connectors the same name and
  puts a button on one can no longer make a new revision of the model.
- **How to check:** two connectors with one name, an integration button on
  one of them, then Build › Models › New revision.
- **What closes it:** open decision: connector names unique per revision (a
  migration and a 409 on create and rename), or copies that carry old-to-new
  ids instead of matching names.

### Schedule rules fire in every revision

- **Noticed:** 2026-09-29, while making revision copies carry schedule rules.
- **What:** the scheduler fires every enabled `schedule` rule whose
  `next_fire_at` has passed, whatever its revision
  (`internal/workflow/scheduler.go:111`), and setting a revision active arms
  or disarms nothing. Revision copies now carry a rule's cron settings but
  leave `next_fire_at` NULL, so the copy is not armed
  (`internal/gateway/handler.go:4572-4577`; the AI `create_revision` runs the
  same statement). Once the copy is set active its schedule rules stay idle
  until someone saves their schedule, while the old revision's rules keep
  firing against the old revision's workflows. Connector schedules
  (`model.integration_schedule`) are not copied at all, so a scheduled
  connector in a new revision runs only by hand until it is scheduled again;
  nothing documents that.
- **Why it matters:** after go-live, scheduled work keeps running on the
  superseded revision and not on the live one.
- **How to check:** a schedule rule, New revision, Set active on the copy,
  then `SELECT revision_id, next_fire_at FROM workflow.automation_rule WHERE
  trigger_type = 'schedule'`.
- **What closes it:** open decision: whether schedules follow the active
  revision (arm the active revision's rules on activation and disarm the
  rest, or have the scheduler fire active revisions only), and the same for
  connector schedules.

### Model export and import drop schedule and connector settings

- **Noticed:** 2026-09-29, while fixing the same fields in revision copies.
- **What:** the package's `AutomationRule`
  (`internal/modeltransfer/transfer.go:241-257`) has no `cron_expr`,
  `timezone`, `misfire_policy`, `max_retries`, `retry_backoff_seconds` or
  `source_integration_id`, and its `Integration` (`:217-227`) only a name, a
  type, a target and the config. Import (`:2002-2006`, `:2035-2037`)
  therefore brings a push connector back as `pull`, with the column defaults
  (status `active`, no tags, description, connection or `config_version`, no
  test result), and a connector-scoped rule with no source, so it fires on
  every connector's run. A schedule rule arrives without `cron_expr`, which
  `automation_rule_schedule_cron_chk` refuses
  (`migrations/059_scheduled_automation.sql:33-34`): read, not run, but a
  package holding a schedule rule most likely cannot be imported. Kept by
  decision (2026-09-29): a package made before 2026-09-29 whose Rate metric
  has no operands is refused with 400 ("needs both a numerator and a
  denominator metric"); adding `agg_numerator_metric_id` and
  `agg_denominator_metric_id` to its JSON imports it.
- **Why it matters:** a model moved to another tenant loses its connectors'
  settings and its schedules.
- **How to check:** export a revision holding a draft push connector and a
  schedule rule, import it, and read the answer and `direction`, `status`
  of the imported `model.integration_def`.
- **What closes it:** carrying those fields in the package.
  `connection_id` belongs to an application: map it by connection name in
  the target application, or leave it empty and import the connector as a
  draft.

### Business roles may be named like platform roles

- **Noticed:** 2026-09-29, while giving workflow assignment one predicate.
- **What:** nothing refuses a business role called `developer`,
  `tenant_admin` or any other `identity.user_role` value, on create or on
  rename: the business-admin routes (`internal/gateway/handler.go:15077`,
  `:15152`), the AI (`internal/aiassistant/write_executor_workflow.go:383`,
  `write_executor_edit.go:824`) and SCIM, which names roles after the
  identity provider's groups (`ee/scim/service.go:760`, `:797`,
  `ee/scim/patch.go:238`). Workflow assignment now treats such a role as
  naming nobody (`internal/workflow/assignee/assignee.go`): a step naming
  `tenant_admin` means the platform role.
- **Why it matters:** a business admin who creates a role called
  `developer` sees steps assigned to it go to the platform developers, and
  its members are never assigned, without any message.
- **How to check:** on each deployment, `SELECT workspace_id, name FROM
  identity.business_role WHERE name IN (SELECT
  unnest(enum_range(NULL::identity.user_role))::text);`.
- **What closes it:** open decision: reserve those names on create and
  rename on all three paths, and rename the rows the query finds.

### Some workflow notifications reach disabled accounts, and a failed recipient query is silent

- **Noticed:** 2026-09-29, while giving workflow assignment one predicate.
- **What:** role recipients and SLA reminders now leave out disabled
  accounts (`assignee.SQL`). Three paths notify one known user without that
  check: a notification step's "requester" recipient
  (`internal/workflow/store.go:1409-1413`), the rework notice to the starter
  (`:1004`) and `notifyStartFailure` (`:2805`). `notification.Store.Notify`
  (`internal/notification/store.go:228`) checks nothing either, and the
  dispatcher's `claimOutbound` (`internal/notification/dispatch.go:397`)
  e-mails or posts rows queued before the recipient was disabled.
  Separately, `resolveNotificationRecipients` returns no recipients when its
  query fails (`internal/workflow/store.go:1428-1431`), so a broken query
  notifies nobody and logs nothing.
- **Why it matters:** a person removed from the tenant can keep receiving
  workflow e-mails, which carry the workflow's name and message; a recipient
  query broken by a schema change fails unseen.
- **How to check:** disable the starter of an instance, send one of its
  steps back for rework, and look for an e-mail row queued for them.
- **What closes it:** `Notify` skipping a disabled recipient,
  `claimOutbound` dropping or cancelling their pending rows, and the
  recipient query's error logged.

### An overdue step that names no role reminds nobody

- **Noticed:** 2026-09-29, while giving workflow assignment one predicate.
- **What:** a step with no `assignee_roles` sits in the inbox of everyone
  who reaches its application, but the SLA reminder finds no recipients for
  it (`internal/notification/reminder.go:188-190`). Kept rather than start
  reminding a whole workspace.
- **Why it matters:** an overdue unassigned step, such as a condition step
  waiting for a person, is never chased.
- **How to check:** a step with `sla_hours` and no assignee role, past its
  due time: no reminder row is written.
- **What closes it:** a product decision on whom such a step reminds, if
  anyone (for example the application's business admins).

### Approved numbers are released by any step after the approval

- **Noticed:** 2026-09-29, writing the Business admin guide.
- **What:** `writeguard` keeps the numbers of a finished workflow instance
  locked only when the instance's last decided step, by completion time,
  decided `approve` (`internal/writeguard/writeguard.go:243-249`, `:299`).
  A notification step records `sent` or `skipped`, and every later step a
  decision of its own, so a workflow such as "approve, then notify"
  releases the numbers as soon as the notification step finishes. The
  Business admin guide tells readers the lock holds only "if the approval is
  its last step".
- **Why it matters:** the usual shape of an approval (approve, then tell the
  requester) does not keep the agreed figures locked.
- **How to check:** a workflow of an approval step then a notification step
  over some cells; approve it, then write one of those cells: 200.
- **What closes it:** locking a finished, not cancelled instance whose last
  approval step approved, whatever notification, task or condition steps
  follow it; then the qualifier can leave the guide
  (`internal/starter/business_admin.go`).

### Build edits the live revision without saying so

- **Noticed:** 2026-09-29, writing the Developer guide and the tour.
- **What:** with nothing picked in Build › Models, Build works in the active
  revision of the model the server resolves
  (`web/src/consoles/developer/DeveloperConsole.tsx:64-69`), and the top
  bar's revision badge has the draft tone whichever revision it names
  (`:159`). The only revision guard is on delete. Business users see an edit
  to the live revision at once.
- **Why it matters:** the guides tell developers to make a new revision
  first, but the console neither warns nor shows that they are editing the
  live one.
- **How to check:** on a new tenant, open Build, change a metric, and read
  the badge; the change shows in Run.
- **What closes it:** the badge in the live tone when the working revision
  is the active one, and a product decision on a warning or a confirmation
  for edits to it.

### Build edits a system-managed revision's members and definitions

- **Noticed:** 2026-09-29, reviewing the member reorder; read, not run.
- **What:** a `system_managed` revision is filled only by an
  approval-triggered copy and refuses cell writes and imports
  (`writeguard.SystemManaged`, `internal/writeguard/writeguard.go:186`).
  The only gateway caller outside the write guard is the fact import
  (`internal/gateway/handler.go:7164`). The member routes (`POST`, `PATCH`,
  `DELETE /api/developer/dimensions/{dimId}/members…` and the reorder,
  `:356-360`) and the other definition edits in Build do not check it, and
  a member delete or first child moves or removes that revision's facts
  (`internal/modeledit/modeledit.go:160`, `:264`).
- **Why it matters:** a revision meant to hold an approved copy can be
  changed in Build, after the approval that produced it.
- **How to check:** mark a revision `system_managed`, pick it in Build and
  add a member: 200.
- **What closes it:** the definition edits refusing a `system_managed`
  revision through one shared check (the member routes, the reorder, the
  metric, dimension, grid and form edits, and the AI Developer's tools),
  with a test.

### A developer who is also a business admin cannot grant a working revision's dashboards

- **Noticed:** 2026-09-29, writing the Developer guide. Standing rule 2.
- **What:** Build › Roles is hidden from anyone who holds `business_admin`
  (`web/src/consoles/developer/DeveloperConsole.tsx:130`), and Business
  Admin › Roles saves grants with no revision, so they apply to the selected
  model's live revision
  (`web/src/consoles/business-admin/BusinessAdminConsole.tsx:329-333`). A
  dashboard that is new in the working revision can be granted only after
  Set active, and until someone does, no member of any business role sees
  it (people in no role do). A developer without `business_admin` can grant
  it beforehand.
- **Why it matters:** a real role cannot prepare go-live, and a new
  dashboard is hidden from every role at the moment it goes live.
- **How to check:** sign in with `developer` and `business_admin`, add a
  dashboard in a working revision, and look for a way to grant it.
- **What closes it:** a decision, since it touches the one-console rule:
  Business Admin › Roles offering the working revision to a developer, or
  Build › Roles shown, scoped to the working revision, when `business_admin`
  is held too.

### A new revision takes its dashboard grants from its source revision

- **Noticed:** 2026-09-29, writing the Business admin guide.
- **What:** a revision copy carries dashboard grants from its source
  revision, matched by dashboard name (step K,
  `internal/gateway/handler.go:4784-4799`). Build › Models › New revision now
  copies the working revision when there is one. If a business admin changed
  grants on the live revision after that working copy was made, a revision
  made from the working copy carries the older grants, and setting it
  active applies them.
- **Why it matters:** a grant or revocation made on the live revision can be
  undone without notice at the next go-live.
- **How to check:** make a working copy, change a grant on the live
  revision, make a new revision from the working copy, and compare grants.
- **What closes it:** a decision on which grants a copy takes (for example
  the live revision's, for the dashboards that exist there).

### Two rule types cannot be attached to a workflow from Build › Triggers

- **Noticed:** 2026-09-29, writing the tour. Standing rule 2.
- **What:** a rule needs a workflow
  (`web/src/consoles/developer/AutomationTab.tsx:102`). An event workflow
  fixes the trigger type to the one its start event gives
  (`triggerTypeFromWorkflow`, `workflowConstants.ts:12-18`), never
  `form_approval` or `grid_change`, and a manual workflow limits the choice
  to manual or schedule (`AutomationTab.tsx:283-286`, `:327-335`). The
  start-event catalogue has no approval or grid-change event. The engine
  dispatches both types (`internal/gateway/handler.go:2001`, `:10329`), so
  they are reachable only through the API or the AI.
- **Why it matters:** the Triggers screen lists two types a developer
  cannot use.
- **How to check:** Build › Triggers › New rule, pick any workflow, and open
  Trigger type.
- **What closes it:** approval and grid-change start events in the
  catalogue, so an event workflow can carry them, or the full choice for a
  manual workflow.

### A published manual workflow shows "No rule yet" as a warning

- **Noticed:** 2026-09-29, writing the Developer guide.
- **What:** the workflow editor shows a warning-toned "No rule yet" badge
  whenever no automation rule uses the workflow
  (`web/src/consoles/developer/WorkflowEditor.tsx:252-253`), including a
  published manual workflow, which people start by hand without any rule.
- **Why it matters:** it presents a working setup as a problem.
- **How to check:** publish a manual workflow with no rule and open it.
- **What closes it:** no badge, or a neutral one, for a manual workflow.

### A business admin who hides a row from themselves cannot undo it

- **Noticed:** 2026-09-29, writing the Business admin guide.
- **What:** Business Admin › Access Rules lists members and metrics through
  `baAvailable`, which leaves out what is hidden from the calling admin,
  and everything under a hidden member
  (`internal/gateway/handler.go:14907-14988`). A business admin who sets
  Hidden on themselves loses that row from the screen. The AI's
  `set_user_access_rules` changes member rules only, and the access-rule
  routes are business-admin only, so a metric hidden this way can be
  restored only by another business admin.
- **Why it matters:** on a new tenant the sign-up owner is the only business
  admin: the metric stays hidden until a tenant admin invites a second one.
  The guide warns readers off it.
- **How to check:** as a business admin, hide a metric from yourself and
  reopen your own rules.
- **What closes it:** listing the caller's own hidden rows when they edit
  their own rules, or refusing Hidden on oneself.

### Access-rule changes reach an open grid only on its 30-second refresh

- **Noticed:** 2026-09-29, writing the Business admin guide.
- **What:** saving rules refreshes only the rules list
  (`["ba-access-rules", user]`,
  `web/src/consoles/business-admin/BusinessAdminConsole.tsx:651`). An open
  grid picks the change up through grid-meta's 30 s stale time and refetch
  (`web/src/consoles/business/PlanningGrid.tsx:477`, `web/src/main.tsx:13`),
  even in the admin's own browser. The guide says "within half a minute, or
  at once if you reload".
- **Why it matters:** an admin who changes their own rules sees the old ones
  for up to half a minute.
- **How to check:** change your own rules with a grid open in another tab.
- **What closes it:** invalidating the grid-meta and KPI queries when rules
  are saved; other users' grids keep the 30 s refresh.

### History › Pending Actions ignores required comments and completion labels

- **Noticed:** 2026-09-29, writing the Business admin guide.
- **What:** Business Admin › History offers "Add a comment (optional)" and
  the default buttons for every open step
  (`web/src/consoles/business-admin/BusinessAdminConsole.tsx:263-271`), where
  the Workflow Inbox passes the step's `required_comment` and
  `completion_label` (`:84-93`). Deciding a step that requires a comment
  there fails with "a comment is required to complete this step".
- **Why it matters:** the screen invites an action it then refuses.
- **How to check:** a step with a required comment, decided from History
  with no comment.
- **What closes it:** History showing both, as the Inbox does (and the
  history endpoint returning them if it does not).

### Workflow History and the Workflow Inbox are scoped differently

- **Noticed:** 2026-09-29.
- **What:** History (`GET /api/workflow/history`,
  `internal/gateway/handler.go:5451`) lists the instances the caller
  administers, narrowed to the `X-App-Id` application when one is sent. The
  console sends the application kept in `localStorage` (`selected_app_id`),
  which Business Admin › Models › Open sets, but so does the developer's
  application auto-pick. The Workflow Inbox (`/api/tasks`) does not read
  `X-App-Id`.
- **Why it matters:** History and the Inbox can show different sets of
  requests, and an account that is also a developer can find History
  narrowed to an application it never chose there.
- **How to check:** a workspace with two applications that both have
  running requests, one of them open.
- **What closes it:** a decision whether History follows the selected
  application; if it does, the screen naming it.

### Tenant-level applications have no business-role workspace of their own

- **Noticed:** 2026-09-29.
- **What:** an application with no workspace (`workspace_id` NULL, the shape
  `POST /api/admin/applications` creates) belongs to every workspace of its
  tenant. Business Admin resolves it to the workspace where the caller holds
  `business_admin` (`baWorkspaceModelFor`,
  `internal/gateway/handler.go:14692`), so business admins of two workspaces
  see different roles, users and rules for the same application, and its
  workflow steps match named business roles of any workspace of the tenant.
- **Why it matters:** in a tenant with several workspaces, who administers
  such an application is not defined. A self-service sign-up has one
  workspace and is not affected.
- **How to check:** two workspaces with a business admin each, one
  tenant-level application; compare their Roles screens.
- **What closes it:** a decision: give such applications a workspace, or
  define whose roles govern them.

### A plain developer cannot build workflows on a model-less application

- **Noticed:** 2026-09-29, checking the developer-route change; confirmed by
  a throwaway gateway test (not kept).
- **What:** a developer of the tenant gets 403 "application is outside your
  access scope" on `GET /api/developer/workflows?application_id=` for an
  application with no model (an execution-mode application), though the
  developer's application list shows model-less applications. The cause is
  the `EXISTS (SELECT 1 FROM core.model …)` clause in `actorCanAccessApp`'s
  developer arm (`internal/gateway/handler.go:1431-1440`). Only a tenant
  admin who is also a developer of that tenant, a platform admin or a
  global builder can build workflows and automation rules there.
- **Why it matters:** the role that builds workflows cannot build them on
  the applications made to run workflows.
- **How to check:** as mm-dev in a `setupDevRouteFixture` test, `GET
  /api/developer/workflows?application_id=<a model-less tenant-1
  application>`.
- **What closes it:** the user's decision: opening model-less applications
  to the tenant's developers widens a shared predicate. If agreed, the
  model clause applies only when the application has a model, with a test.

### The application picker offers applications the caller cannot build in

- **Noticed:** 2026-09-29.
- **What:** `/api/apps` lists every application a business role opens. A
  developer or tenant admin whose business role is in another tenant can
  pick that tenant's application, and the developer and admin groups then
  answer 403 there, since their routes count builder and admin reach only.
  The same holds in Build › Models since the developer routes were narrowed
  (2026-09-29): `GET /api/developer/applications` (`devOrAdm`,
  `adminTenants`, `internal/gateway/handler.go:341`) still lists the
  applications, models and revisions of a tenant where the account is only
  `tenant_admin`, and every Build route answers 403 for them. They are
  reached by a click, by the newest-revision fallback
  (`web/src/consoles/developer/ApplicationsTab.tsx:185-200`), or by a stale
  `selected_app_id`; New revision and Set default fail there too. Only the
  cross-tenant shape (developer in A, `tenant_admin` in B) is affected; an
  owner is not.
- **Why it matters:** the console offers screens that cannot work.
- **How to check:** a developer of tenant 1 who is a business user in tenant
  2; pick tenant 2's application and open Build. For Build › Models,
  `internal/gateway/developer_route_scope_test.go` asserts that app3 is
  listed to the cross accounts (`:210`), while
  `TestDeveloperRoutesIgnoreAdminScopeElsewhere` asserts 403 on `GET
  /api/developer/model` for app3.
- **What closes it:** per-application capabilities in `/api/apps` and
  `/api/developer/applications` (for example `can_build`, `can_admin`,
  computed with the developer-route rule), `web/src/router/sections.ts`
  hiding the groups the user cannot use there, and Build › Models hiding or
  disabling admin-only entries and leaving them out of the fallback, with a
  Vitest or Playwright check.

### A future developer-only handler with a fresh context loses the route markers

- **Noticed:** 2026-09-29, narrowing the developer routes.
- **What:** `dev()` marks its requests with `developerRouteKey` and
  `builderRouteKey` in the request's context
  (`internal/gateway/handler.go:1294-1311`). The scope checks read the
  marker from the context they are given: `requireResourceAccess`,
  `reorderDimensionMembers`, `actorCanAccessApp`/`actorCanAccessModel` and
  `resolveDemoModelID`/`resolveDemoAppID`. Every developer-route scope
  check found today passes `r.Context()` or a context derived from it
  (checked: `developerRevisions`, `developerSetDefaultModel`,
  `developerRevisionAction` and the AI session handlers); the gateway's
  seven `context.Background()` calls are recalculation and migration
  goroutines that check no scope.
- **Why it matters:** a handler that built a fresh context before calling
  these checks would lose both markers and be judged as a business route,
  where a role held in another tenant counts differently.
- **How to check:** `grep -n 'context.Background()' internal/gateway/*.go`,
  and for each hit whether an access predicate follows.
- **What closes it:** a lint or test that fails when a handler behind
  `dev()` calls an access predicate with a context not derived from the
  request's; or passing the route kind explicitly.

### Per-user application grants do not narrow a tenant owner's build reach

- **Noticed:** 2026-09-29, decided while narrowing the developer routes.
- **What:** on developer-only routes, an account that is `tenant_admin` and
  also a developer of the same tenant (the sign-up owner) keeps what its
  admin grant opens there (`builderAdminScope`,
  `internal/gateway/handler.go:1335`): applications with no model, and
  applications outside its `user_app_access`/`user_model_access` grants.
  Those grants narrow only the developer arms. This is the behaviour from
  before the change, restored on purpose and pinned by
  `TestDeveloperRoutesKeepOwnerAdminReach`
  (`internal/gateway/developer_route_scope_test.go:237`): ws-owner with a
  grant to app1 only can still `PATCH` app2's metric.
- **Why it matters:** an administrator who narrows an owner's applications
  narrows what the owner sees in the business console, not what it builds.
- **How to check:** run that test.
- **What closes it:** nothing while the decision stands. If grants should
  narrow owners too, change that test, and make `/api/apps` and
  `/api/developer/applications` list the same set to such accounts.

### Rules and workflows requested without a revision come from the oldest model

- **Noticed:** 2026-09-29, fixing Triggers in a multi-model application.
- **What:** for automation rules, workflow definitions and the trigger-event
  catalogue, a request with no `?revision_id=` resolves the application's
  oldest model (`resolveAppRevisionID`, `appWorkingModelQuery`,
  `internal/gateway/handler.go:9348-9387`) and ignores `X-Model-Id`, while
  forms, grids and integrations follow `X-Model-Id`. On a signed-up tenant
  the oldest model is the tour. The console's Triggers and workflow screens
  now always send the working revision; an API client that sends none gets
  the tour's rules while it works in a guide.
- **Why it matters:** two resources read by one request can come from
  different models.
- **How to check:** `GET /api/automation/rules` with `X-Model-Id` of a guide
  model and no `revision_id`.
- **What closes it:** a decision: honouring `X-Model-Id` there changes which
  rules existing multi-model applications show.

### The form-to-metric mapping section lists another model's items

- **Noticed:** 2026-09-29, while scoping the import wizard to the working
  revision.
- **What:** Build › Integrations' form-to-metric mappings
  (`FormRecordsSection`,
  `web/src/consoles/developer/IntegrationsTab.tsx:309-312`) load forms, the
  model, dimensions and grids with no revision, so in a multi-model
  application they come from the `X-Model-Id` or default model rather than
  the working revision's; and `GET /api/developer/grids` with no revision
  lists every revision's grids (`internal/gateway/handler.go:12992`). The
  import wizard beside it now follows the working revision.
- **Why it matters:** a developer working in a guide is offered the tour's
  forms and metrics.
- **How to check:** pick a guide's revision in Build › Models and open the
  mapping form.
- **What closes it:** the same working-revision scoping as the import
  wizard.

### Trigger-event keys made from names can collide

- **Noticed:** 2026-09-29.
- **What:** per-form and per-integration start events are keyed by the
  name, lower-cased with spaces and hyphens turned into underscores
  (`toEventKey`, `internal/gateway/handler.go:16064`). An integration named
  "integration" gives `integration.import.completed`, the system "any
  import" event (`:16188`), which a rule reads as any source; forms named
  "Budget request" and "budget-request" share one key.
- **Why it matters:** a workflow meant for one source starts for others.
- **How to check:** name an integration "integration" and read the
  trigger-event catalogue.
- **What closes it:** keys built from ids, or a name refused when its key is
  taken.

### A copied active connector runs without a test of its remapped configuration

- **Noticed:** 2026-09-29, while making revision copies carry connector
  settings.
- **What:** a revision copy now keeps a connector's status and last test
  (`internal/gateway/handler.go:4539-4559`). Its configuration is remapped
  to the new revision's target, so `last_tested_hash` no longer matches and
  `Definition.Tested()` is false, yet the copy stays `active`. Activation
  requires a test of the current configuration
  (`internal/integration/store.go:350`), but a run checks only status and
  enabled (`internal/gateway/rest_api_integrations.go:394`). Before, the
  copy was active with no test result at all, so this is not a regression.
- **Why it matters:** the tested-configuration gate is not enforced for
  copies, or for runs generally.
- **How to check:** duplicate a revision holding an active connector and run
  the copy.
- **What closes it:** a decision: reset a remapped copy to draft, or accept
  the source's test across a pure target remap; and whether runs enforce the
  gate.

### Connector targets are checked against the model, not the revision

- **Noticed:** 2026-09-29.
- **What:** `integration.CheckOwnership`
  (`internal/integration/ownership.go:54`, the query at `:65`) requires a
  connector's target to be a row of the connector's model, and its
  connection to belong to the model's application. It does not look at the
  revision. The gateway runs it on every save and enqueue, and the worker
  before each run (`internal/integration/runner.go:185`). The web builder
  now sends the working revision
  (`web/src/consoles/developer/api-integrations/ApiIntegrationBuilder.tsx:83`),
  but nothing refuses a grid, form or dimension of another revision of the
  same model: neither a save through the API nor a revision copy whose
  target could not be mapped, which keeps the source's `target_id` (the
  `COALESCE` fallback, `internal/gateway/handler.go:4543-4544`). Seen by
  reading; not yet exercised live.
- **Why it matters:** a working revision's connector can read from or write
  into the active revision's grid.
- **How to check:** `POST /api/developer/integrations?revision_id=<working>`
  with `config.target_id` set to a grid of the active revision: the save
  succeeds, and a run writes into that grid.
- **What closes it:** a decision to require the target in the connector's
  own revision (or a revision-less row) on save and before each run, with
  the copy's unmapped-target fallback cleared.

### Revision copies are written twice

- **Noticed:** 2026-09-29.
- **What:** the gateway's `duplicateRevision` (`internal/gateway/handler.go`,
  steps G to K) and the AI's `create_revision`
  (`internal/aiassistant/write_executor.go:2458`, `:2503`, `:2543`) copy
  connectors, workflows and rules with the same SQL written out twice. They
  had drifted: until this change the AI copy left a workflow's
  `subject_config` on the source revision, and copied connectors before
  forms and dashboards existed.
- **Why it matters:** each fix has to be made twice, and a missed one
  reappears as a copy that points at the source revision.
- **How to check:** compare the two statements.
- **What closes it:** one shared helper, for example in `internal/modeledit`,
  which both packages can import.

### A saved CSV or Sheets import into a form imports nothing

- **Noticed:** 2026-09-29.
- **What:** `integrationRun`'s form branch inserts into `model.form_record`
  (`internal/gateway/handler.go:8306`), a table no migration creates; form
  records live in `runtime.form_record` (`migrations/017_crud_forms.sql:15`).
  Every row fails, and the run answers 200 with `error_rows` counting them.
- **Why it matters:** a saved csv_import or google_sheets integration with a
  form target has never worked, and says so only in its counts.
- **How to check:** `grep -n 'model.form_record' internal/gateway/handler.go`;
  run such an integration: `{"error_rows":1,"rows_imported":0}`.
- **What closes it:** writing through the path the form submit endpoint uses
  (`runtime.form_record`, with its posting and recalculation), and a test
  that a saved CSV-to-form run imports its rows.

### Deleting a form mapping or a form leaves its posted totals in the metric

- **Noticed:** 2026-09-29, reviewing record deletes; read, not run.
- **What:** a form-to-metric mapping keeps its aggregate in
  `runtime.fact_input` rows tagged `source_ref` = the mapping's id, which
  have no foreign key (`migrations/035_form_metric_mapping.sql:45`).
  `DELETE /api/developer/form-integrations/{id}` deletes the mapping
  (`internal/gateway/handler.go:8797-8809`), and deleting a form cascades
  to its mappings (`internal/crudapp/store.go:170-176`); the posting rows
  go, the `fact_input` rows stay, and the grid still sums them. A `PATCH`
  that changes a mapping's target metric leaves the old metric's rows the
  same way: the recompute deletes only the new target's (`:9168`). A
  record delete now takes its postings out (`retractPostings`,
  `internal/gateway/form_record_access.go:193`, pinned by
  `TestFormRecordRetractionTakesValueOut`).
- **Why it matters:** a metric keeps showing money from a form or mapping
  that no longer exists, and nothing in the console can remove it.
- **How to check:** post records through a mapping, delete the mapping,
  and read the metric's cell: the total is unchanged.
- **What closes it:** the mapping delete, the form delete and a retargeting
  `PATCH` deleting the mapping's `source_ref` rows (for the old target) and
  recalculating the metrics that read them, in one shared helper, with a
  test.

### The posting write guard runs as whoever changed the record

- **Noticed:** 2026-09-29, reviewing the form record permissions; read, not
  run.
- **What:** `applyFormMappings` checks `writeguard.MetricAccess` and
  `writeguard.CheckWriteMetrics` for the `userID` it is given
  (`internal/gateway/handler.go:9011`, `:9034`), which is the caller of the
  request: the approver when a business admin approves a record
  (`:10559-10562`), the administrator who runs a sync. The record's
  submitter is not checked.
- **Why it matters:** a submitter restricted from a member or metric can
  have a value posted there by an approver who is not; and an approver
  restricted there withholds a submitter's posting without saying so.
- **How to check:** a business user with a read-only access rule on member
  M submits a record naming M; a business admin with no rule approves it;
  the value is posted.
- **What closes it:** a decision whose access a posting follows (the
  submitter's, the approver's, or both), then one rule in
  `applyFormMappings` with a test.

### Form import creates submitted and approved records without their rules

- **Noticed:** 2026-09-29, reviewing the form record permissions; read, not
  run.
- **What:** `POST /api/forms/{id}/import` creates each record in the status
  it is allowed (`internal/gateway/form_transfer.go:277-286`) and posts it,
  but does not call `DispatchEventRules`. A record created or moved through
  `POST /api/forms/{id}/records` or `PUT /api/records/{id}` fires the
  form's `form_submit` and `form_approval` rules
  (`internal/gateway/handler.go:10389-10396`, `:10565-10571`).
- **Why it matters:** an automation that starts a workflow for each
  submitted expense does not start for imported ones.
- **How to check:** a `form_submit` rule on a form; import a CSV of two
  submitted records; no instance starts.
- **What closes it:** the import dispatching the same rules per record as
  the create does, or a decision that an import is silent, said in the
  manual.

### Form record field labels are not tied to their inputs

- **Noticed:** 2026-09-29, writing the form record Playwright specs.
- **What:** `Field` clones a generated id onto its single child
  (`web/src/ui/Field.tsx:41`), but `FormFieldInput`
  (`web/src/consoles/business/FormsTab.tsx:33`) neither accepts nor
  forwards `id`, so the label (for example "Vendor") is not tied to the
  input or select it renders, in Run › Forms and in the dashboard form
  widget (`web/src/consoles/business/DashboardWidgets.tsx`).
- **Why it matters:** screen readers do not announce the label, and
  `getByLabel('Vendor')` does not find the input.
- **How to check:** open a form record editor and inspect the input: no
  `id` matching the label's `for`.
- **What closes it:** `FormFieldInput` taking an `id` and passing it to the
  element it renders.

### The forms list can show a form whose records the caller does not reach

- **Noticed:** 2026-09-29, adding permissions to the forms list; not
  checked either way.
- **What:** `GET /api/forms` lists the forms of the model
  `resolveDemoModelID` resolves (`internal/gateway/handler.go:10068`), while
  the record routes decide by `resolveFormRecordScope`
  (`internal/gateway/form_record_access.go:45`). Where the second says the
  caller does not reach a listed form, the UI hides New record and the
  records list answers 404. Whether the two decisions can differ (per-user
  application or model grants, the application header) was not checked.
- **Why it matters:** a user could see a form tab with no records and no
  actions.
- **How to check:** a test where a user's access grants exclude the model
  but the application header still resolves it.
- **What closes it:** the forms list leaving out forms the scope does not
  reach, or showing them that way on purpose.

### The cell write accepts a value on a parent member

- **Noticed:** 2026-09-29, by the tour's reviewer; not re-run.
- **What:** `POST /api/cells` does not check that each member is a leaf;
  `writeguard.IsLeafMember` (`internal/writeguard/writeguard.go:77`) has no
  callers. The import path refuses a parent member
  (`internal/importpkg/resolve.go:238`). The reviewer reports that a value
  stored on a parent is ignored by the grid but counted by a pinned KPI.
- **Why it matters:** the grid and a KPI can disagree about the same total.
- **How to check:** `POST /api/cells` with a parent member's code, then read
  the grid and a KPI pinned to that member.
- **What closes it:** refusing a non-leaf member in `cells()`, as the import
  does.

### The cell write stores a value for a member code that does not exist

- **Noticed:** 2026-09-29, reviewing the cell write; read, not run.
- **What:** `POST /api/cells` resolves each `dim_codes` entry to a member
  for the write guard and skips an unknown code ("nothing to restrict
  here", `internal/gateway/handler.go:2057`), then inserts the fact with the
  codes as sent (`:2090-2094`). The gRPC `QueryService.Writeback` does the
  same (`internal/query/store.go:74`, insert at `:107`), and so does a form
  posting whose record names an unknown member
  (`internal/gateway/handler.go:9028`). The dimension ids in `dim_codes`
  are not checked against the revision either. The import refuses an
  unknown code (`internal/importpkg/resolve.go:229`).
- **Why it matters:** the fact is stored but no grid, chart or total reads
  it, and it counts against the plan's fact rows. A member created later
  with that code picks the value up, though no write guard checked it
  against that member.
- **How to check:** `POST /api/cells` with `dim_codes` naming a code the
  dimension does not have: 200, and a `runtime.fact_input` row with that
  code.
- **What closes it:** one shared resolver for `cells()`, the gRPC write and
  the form posting that refuses an unknown member code, and a dimension
  that is not the revision's, with a 400 naming them, as the import does.

### Grid layouts copied before 2026-09-29 name the source revision's dimensions

- **Noticed:** 2026-09-29, while making revision copies remap widget
  layouts.
- **What:** until this change, revision copies, the AI copy and model import
  copied a grid widget's saved layout (`widget_props.default_view`: rows,
  columns, context and the `filter_sel` keys) as it was. Those widgets name
  the source revision's dimension ids: they render with those dimensions
  dropped from the layout and their saved filters ignored. Copies made now
  are remapped (`modeltransfer.RemapWidgetPropsIDs`), and export resolves
  such a reference to this revision's copy by lineage, but the stored rows
  are unchanged.
- **Why it matters:** revisions copied earlier show grids laid out
  differently from their source.
- **How to check:**
  ```sql
  SELECT w.id FROM model.dashboard_widget w
  JOIN model.dashboard_def d ON d.id = w.dashboard_id
  WHERE w.widget_props ? 'default_view' AND EXISTS (
    SELECT 1 FROM jsonb_array_elements_text(
        COALESCE(w.widget_props->'default_view'->'rows', '[]')
     || COALESCE(w.widget_props->'default_view'->'cols', '[]')
     || COALESCE(w.widget_props->'default_view'->'context', '[]')) x
    WHERE x <> '__metrics__' AND NOT EXISTS (
      SELECT 1 FROM model.dimension_def dd
      WHERE dd.id::text = x AND dd.revision_id = d.revision_id));
  ```
  (the `COALESCE`s matter: without them a widget missing one axis is
  skipped), and the same for the `filter_sel` keys.
- **What closes it:** a backfill that maps revision copies through
  `dimension_def.lineage_id` (migration 099); an imported model's layouts
  saved again by hand.

### Model import finds JSON references by field name and drops some links without a message

- **Noticed:** 2026-09-29, while making import resolve every reference.
- **What:**
  - Row ids inside JSON documents (widget props, form fields, context,
    connector config) are recognised only in named positions: fields ending
    `_id`, arrays ending `_ids`, the keys of known dimension-keyed objects
    and a saved layout's axes (`jsonRefs`,
    `internal/modeltransfer/transfer.go:1008-1030`). A future field holding
    a row id under another name is copied through unchanged, and nothing
    fails when one is added. Workflow steps are not walked: today they name
    roles and context keys only.
  - A reference that resolves to nothing in the package is dropped without
    a message for a dashboard's folder, a rule's workflow, source form and
    source grid, a grid's metrics and dimensions, dependencies, a mapping's
    form and metric, and a fact's metric and source mapping. A hand-written
    package with a typo loses the link silently.
  - A member of a standard dimension may name a member of another dimension
    of the package as its parent: parents resolve across all dimensions
    (`:1698-1707`), and `timedim.ValidateAndReindex` checks a parent's
    dimension only for time dimensions. Read, not run.
  - The package's `format` field and its format number (`:269-270`) are not
    checked.
- **Why it matters:** each is a way for an import to succeed with less, or
  other, than the package said.
- **How to check:** import a package with each case.
- **What closes it:** a test that lists the known JSON keys and fails on a
  new key holding an id (or typed schemas for these documents); warnings for
  dropped references in the import's answer; a same-dimension parent check;
  refusing an unknown format.

### The AI Developer remaps only a widget's chart fields

- **Noticed:** 2026-09-29, while making revision copies remap widget props.
- **What:** `remapChartProps` (`internal/aiassistant/write_executor.go:205`,
  called at `:1938` and `write_executor_edit.go:541`) resolves ids into the
  working revision for `chart.dimension_id`, `x_metric_id`, `y_metric_id`
  and `metric_ids` only. `kpi_scope.dimension_id`, the
  `chart.context_defaults` keys, `default_view` and `filter_sel`, which
  revision copies now remap (`RemapWidgetPropsIDs`), are saved as given.
- **Why it matters:** an AI-proposed grid layout or KPI scope naming another
  revision's ids is saved unmapped.
- **How to check:** ask the assistant to add a KPI scoped by a dimension of
  another revision and read the saved `widget_props`.
- **What closes it:** `remapChartProps` resolving the same fields through
  `requireInModel`.

### Grid, KPI and chart format the same metric differently

- **Noticed:** 2026-09-29, writing the Developer guide.
- **What:** for Percentage, the grid appends "%" to the stored value
  (`web/src/consoles/business/PlanningGrid.tsx:1477-1478`), a KPI multiplies
  by 100 (`web/src/consoles/business/DashboardWidgets.tsx:244`) and a chart's
  percent format uses `Intl` `percent`, which multiplies too
  (`web/src/consoles/dashboard/chartTypes.ts:19`): 0.25 reads 0.25% in a
  grid and 25.0% on a KPI. For Currency, a KPI shows no decimals whatever
  `format_decimals` says (`DashboardWidgets.tsx:242`): $3.33 reads $3.
- **Why it matters:** no formula scale reads right everywhere. The formulas
  manual's recipes multiply by 100, which suits the grid only; the guides
  avoid the Percentage format for this reason.
- **How to check:** a metric with the Percentage format and value 0.25 on a
  grid and a KPI.
- **What closes it:** one shared formatter with one percentage convention,
  honouring `format_decimals`.

### Nothing sets an existing account's tenant

- **Noticed:** 2026-09-30, closing account adoption at first sign-in.
  Standing rule 2. Narrowed 2026-09-30 (`f0f8986`): a new account invited
  into a workspace now belongs to its tenant, and migration 102 gives most
  accounts made before theirs ("A platform admin's invitation made an
  account with no tenant", Closed below). What is left is below.
- **What:** first sign-in through a tenant's single sign-on onto an account
  that already exists at the address is refused (403, with the reason)
  unless the account belongs to that tenant and is not a platform admin
  (`ee/sso/sso.go:393-415`). Accounts with no `customer_id` remain: those
  migration 102 (`migrations/102_account_tenant_backfill.sql`) leaves alone
  (a platform admin; an account holding `developer` or `tenant_admin`
  without a workspace; one whose identity-provider subject the directory,
  `platform.user_directory`, lists in a tenant database; and one whose
  workspace roles, business-role memberships and application and model
  grants lie in several tenants, or in none), a platform admin's creation
  with no workspace, and an account a platform admin's creation adopts,
  which keeps what it had (`internal/gateway/handler.go:13071-13089`). No
  route or screen sets an existing account's `customer_id`, so no role can
  make such an account sign in through its tenant's single sign-on. Adding
  an existing account to a tenant (`addExistingAccount`,
  `internal/gateway/account_boundaries.go:579`) leaves its tenant as it is
  too.
- **Why it matters:** a person invited before their tenant set up single
  sign-on may be locked out of it, with no way back through the product.
- **How to check:** `grep -rn 'SET customer_id' --include='*.go' internal
  ee` finds nothing outside tests. The accounts with no tenant, by why 102
  left them, read-only on the shared database (in a tenant database 102
  writes nothing):
  ```sql
  SELECT count(*) FILTER (WHERE pa) AS platform_admins,
         count(*) FILTER (WHERE NOT pa AND unscoped_builder) AS unscoped_dev_or_ta,
         count(*) FILTER (WHERE NOT pa AND NOT unscoped_builder AND in_directory) AS in_a_tenant_database,
         count(*) FILTER (WHERE NOT pa AND NOT unscoped_builder AND NOT in_directory AND tenants = 0) AS no_roles,
         count(*) FILTER (WHERE NOT pa AND NOT unscoped_builder AND NOT in_directory AND tenants > 1) AS several_tenants,
         count(*) FILTER (WHERE NOT pa AND NOT unscoped_builder AND NOT in_directory AND tenants = 1) AS backfilled_by_102
  FROM (
    SELECT EXISTS (SELECT 1 FROM identity.role_assignment ra WHERE ra.user_id = u.id AND ra.role = 'platform_admin') AS pa,
           EXISTS (SELECT 1 FROM identity.role_assignment ra WHERE ra.user_id = u.id AND ra.workspace_id IS NULL
                     AND ra.role IN ('developer', 'tenant_admin')) AS unscoped_builder,
           EXISTS (SELECT 1 FROM platform.user_directory d WHERE d.keycloak_sub = u.keycloak_sub) AS in_directory,
           (SELECT count(DISTINCT c) FROM (
               SELECT w.customer_id AS c FROM identity.role_assignment ra JOIN core.workspace w ON w.id = ra.workspace_id WHERE ra.user_id = u.id
               UNION SELECT w.customer_id FROM identity.business_role_member m JOIN identity.business_role br ON br.id = m.role_id
                     JOIN core.workspace w ON w.id = br.workspace_id WHERE m.user_id = u.id
               UNION SELECT COALESCE(a.customer_id, aw.customer_id) FROM identity.user_app_access ua JOIN core.application a ON a.id = ua.application_id
                     LEFT JOIN core.workspace aw ON aw.id = a.workspace_id WHERE ua.user_id = u.id
               UNION SELECT COALESCE(a.customer_id, aw.customer_id) FROM identity.user_model_access um JOIN core.model mm ON mm.id = um.model_id
                     JOIN core.application a ON a.id = mm.application_id LEFT JOIN core.workspace aw ON aw.id = a.workspace_id WHERE um.user_id = u.id
           ) held) AS tenants
    FROM identity."user" u WHERE u.customer_id IS NULL
  ) s;
  ```
  Run 2026-09-30 on the local development database, before 102 was
  applied: 1 platform admin, 0 unscoped developer or tenant_admin, 0 listed
  in a tenant database, 3 with no roles, 0 in several tenants, and 5 that
  102 backfills. Not yet run on any deployment.
- **What closes it:** a platform-admin action that sets an account's tenant,
  audited; or the refusal naming what to do instead.

### A SCIM delete of a member the tenant does not own leaves the member's access

- **Noticed:** 2026-09-30, limiting SCIM writes to the tenant's own
  accounts.
- **What:** a tenant's SCIM token lists everyone who holds a role in its
  workspaces, but changes and deletes only the tenant's own accounts, never a
  platform admin (`ownedSQL`, `ee/scim/service.go:257`). For the others it
  answers 403 (a request that changes nothing is answered as it is), so an
  identity provider that pushes changes for them logs 403s, and a SCIM delete
  no longer removes such a member's access in the tenant: the tenant admin
  removes it with Remove from this tenant on the Users screen (`DELETE
  /api/admin/users/{id}/tenant-access`, since 2026-09-30).
- **Why it matters:** off-boarding through the identity provider leaves
  these members' roles in place until an administrator removes them.
- **How to check:** `TestSCIMChangesOnlyTheTenantsOwnAccounts`
  (`internal/gateway/global_builder_escalation_test.go`).
- **What closes it:** a SCIM delete of a member the tenant does not own
  removes what that member holds in this tenant, as Remove from this tenant
  does (`removeTenantAccess`, `internal/gateway/account_boundaries.go:351`),
  and leaves the account.

### Inviting an existing address still shows afterwards that it had an account

- **Noticed:** 2026-09-30, adding existing accounts to a tenant.
- **What:** `POST /api/admin/users` answers alike for a new address and an
  existing account, but what follows does not. The users list shows an added
  account with its own display name and creation date and `home_tenant`
  `other` or `none`, where a new one is `own`, and the Users screen then
  shows the foreign-account note. The grant's audit event, which the
  tenant's administrators read (`auditScope`,
  `internal/gateway/handler.go:7064`), carries `existing_account=true`. And
  an existing account is answered before any identity-provider call, where a
  new invitation waits for the identity provider and the invitation's mail
  (`internal/gateway/handler.go:12990-13128`). Open self-service sign-up
  answers 409 for an address that has an account
  (`internal/gateway/signup.go:114-122`), so where it is open the flow tells
  a tenant admin nothing an unauthenticated caller cannot learn there. An
  invitation with no role or no workspace is now refused before the address
  is looked at, alike for both ("An existing account invited without a
  workspace is told it was invited, and nothing happens", Closed below).
  Re-checked 2026-09-30 against the uncommitted change: unchanged.
- **Why it matters:** the decision that an invitation does not reveal an
  existing account holds for the response only.
- **How to check:** invite another tenant's user into a workspace, then `GET
  /api/admin/users` and `GET /api/admin/audit`.
- **Decision:** accepted by the user on 2026-09-30: the `existing_account`
  flag in the grant's audit event, which the tenant's administrators read,
  and the difference in answer time between an existing account and a new
  address stay as they are. The users list's `home_tenant` was not part of
  the decision; the foreign-account note and Remove from this tenant read
  it.
- **What closes it:** only if that decision changes: the flag left out of
  the event the tenant reads, and the invitation's mail sent asynchronously,
  which conflicts with the rule that a failed invitation rolls the whole
  creation back; sign-up would then have to stop telling too.

### A tenant admin cannot rename or delete a fellow tenant admin

- **Noticed:** 2026-09-30, limiting account-level actions to the caller's
  own accounts. Needs the user's confirmation. Standing rule 2.
- **What:** renaming, deleting or re-inviting an account now also needs the
  caller to be able to revoke every role the account holds
  (`accountPermissionsFor`, `internal/gateway/account_boundaries.go:116`),
  because a delete takes them all. Since 2026-09-30 (`f0f8986`) the
  caller's tier is the one it holds in the account's home tenant
  (`adminTiers.rolesIn`, `internal/gateway/admin_tiers.go:150`). A tenant
  admin may not grant or revoke `tenant_admin` (`assignableRoles`,
  `internal/gateway/handler.go:12609`), so it can no longer rename, delete
  or re-invite a fellow tenant admin, and a developer can no longer do so to
  a developer or a tenant admin. The same holds for an account of the
  caller's tenant that holds, anywhere, a role the caller could not revoke,
  such as `tenant_admin` in another tenant's workspace granted by a platform
  admin. A tenant admin with no tenant of its own (home `none`) can no
  longer rename itself. A caller whose scope is every tenant without being a
  platform admin (a platform-wide builder) keeps the accounts with no
  tenant, but is offered no Remove from this tenant; since 2026-09-30
  (`f0f8986`) its invitations name a workspace and make accounts of that
  workspace's tenant, which it keeps too.
- **Why it matters:** a tenant whose administrator leaves cannot remove them
  without a platform admin.
- **How to check:** `TestAccountActionsNeedEveryRoleRevocable`
  (`internal/gateway/account_boundaries_test.go:434`).
- **What closes it:** the user's confirmation of the rule as it is, or a
  narrower one (for example, a tenant admin may delete a fellow tenant admin
  of its own tenant, audited).

### The plan's user cap refuses adding an account that is already a member

- **Noticed:** 2026-09-30, adding existing accounts to a tenant.
- **What:** `POST /api/admin/users` checks the plan's user cap
  (`CheckUsers`, `internal/gateway/handler.go:12979`) before it looks for an
  existing account, so both paths answer alike. A tenant at its cap gets 402
  for an account that already holds a role in it, whose addition would not
  change the count.
- **Why it matters:** at the cap, the invite form cannot give an existing
  member a role in another workspace; the account's own roles route (`POST
  /api/admin/users/{id}/roles`), which is not capped, still can.
- **How to check:** a tenant at its user limit; invite the address of one of
  its members with a role in another workspace.
- **What closes it:** a decision: counting only accounts new to the tenant
  makes the two paths answer differently at the cap, so the cost may be
  accepted and recorded as intended.

### Migration 102 adds accounts to a tenant's user count

- **Noticed:** 2026-09-30, giving accounts with no tenant the tenant of what
  they hold (migration 102, uncommitted).
- **What:** the plan's user cap counts an account in a tenant by its
  `customer_id` or by a role in one of the tenant's workspaces
  (`CheckUsers`, `internal/plan/enforcer.go:176`). Migration 102
  (`migrations/102_account_tenant_backfill.sql`) sets `customer_id` from
  business-role memberships and application and model grants too, so an
  account whose only link to a tenant was one of those now counts toward
  that tenant's cap.
- **Why it matters:** a tenant can reach or pass its user limit on the
  deployment that applies 102, without adding anyone, and then gets 402 on
  its next invitation.
- **How to check:** per tenant, `CheckUsers`' count before and after 102 is
  applied; the accounts 102 moves are the `backfilled_by_102` column of the
  query under "Nothing sets an existing account's tenant".
- **What closes it:** those counts compared with each tenant's plan before
  102 runs on a deployment, and a limit raised or the new count accepted as
  the right one, recorded here.

### Migration 102 writes no audit event

- **Noticed:** 2026-09-30, writing migration 102 (`f0f8986`).
- **What:** migration 102 gives accounts with no tenant a `customer_id`.
  Audit events are written through `pkg/auditlog`, which a SQL migration
  does not run, so which accounts it moved, and into which tenant, is
  recorded nowhere. Creation records it since the same change: `user.created`
  carries `workspace_id` and `customer_id`
  (`internal/gateway/handler.go:13136`).
- **Why it matters:** an account's tenant decides who administers it. After
  102 a tenant's administrators may rename, delete and re-invite accounts
  they could not before, and the audit log does not say why.
- **How to check:** after 102 has run, nothing tells a backfilled account
  from one created with its tenant; before, the query under "Nothing sets an
  existing account's tenant" counts them.
- **What closes it:** the list of accounts 102 will change kept for each
  deployment before it runs (the same query, listing ids and tenants), or a
  gateway step that audits each account the migration moved.

### Migration 102 tells a tenant database by its name

- **Noticed:** 2026-09-30, writing migration 102 (`f0f8986`).
- **What:** in a tenant's own database every workspace is that tenant's, so
  every account there would pass 102's one-tenant test. 102 writes nothing
  in a database whose name is `tenant_` followed by one of its
  `core.customer` ids without dashes
  (`migrations/102_account_tenant_backfill.sql:71-74`), the naming of
  `tenantdb.DatabaseName` (`pkg/tenantdb/provision.go:26`). Nothing ties the
  two: `TestBackfill102LeavesTenantDatabasesAlone`
  (`migrations/backfill_102_test.go:184`) builds the name itself rather
  than calling `DatabaseName`.
- **Why it matters:** if the naming changes, or a tenant database is
  restored under another name, 102 backfills there and hands every account
  with no tenant in it, identities shared with other tenants included, to
  that tenant's administrators.
- **How to check:** compare `DatabaseName` with the migration's expression.
- **What closes it:** the backfill test building the name with
  `tenantdb.DatabaseName`, so a change to it fails there; or a marker in the
  database that says whose it is, read instead of the name.

### An administrator with no tenant opens some of its granted applications in only some places

- **Noticed:** 2026-09-30, counting roles per tenant (`f0f8986`). Standing
  rule 2.
- **What:** an account with no tenant of its own that holds `tenant_admin`
  without a workspace (a platform admin's creation), narrowed by
  application or model grants, now reaches exactly what its grants name
  and administers nothing there (`customerlessAdminGrantTenantSQL`,
  `internal/gateway/admin_tiers.go:205`). `GET /api/admin/applications`
  and the `/api/admin/tenants` tree list a granted application that has no
  model, but the checks built on `reachSQL` (`rolesReachApp`,
  `internal/gateway/handler.go:1405`, and the `/api/apps` list, `userApps`,
  `:9630`) also want a model it may open, so such an application does not
  open there. The
  workflow history's admin scope (`resolveWorkflowAdminScope`, `:5528`)
  leaves the granted applications out.
- **Why it matters:** low: before the change the account administered the
  whole tenant, so this is a narrowing, not a loss of safety. If the shape
  is used, part of what it was granted is out of its reach.
- **How to check:** `TestCustomerlessAdminGrantIsReachNotAdministration`
  (`internal/gateway/admin_tiers_test.go:104`) covers the reach it has; for
  the gaps, grant such an account an application with no model, open it,
  and read its workflow history.
- **What closes it:** `rolesReachApp` and `resolveWorkflowAdminScope`
  reading the same grant reach as `GET /api/admin/applications`
  (`grantedAppSQL`, `internal/gateway/admin_tiers.go:234`); or a decision
  that such an account is not a supported shape.

### A platform admin's failed re-invitation keeps the new name and e-mail

- **Noticed:** 2026-09-30, keeping an adopted account's row when its
  invitation fails (`f0f8986`).
- **What:** a platform admin's creation over an address or subject that
  already has an account adopts it, and the upsert (`ON CONFLICT
  (keycloak_sub) DO UPDATE SET email, display_name`,
  `internal/gateway/handler.go:13084-13089`) overwrites the account's
  e-mail and display name. When the invitation's mail then fails, the
  request takes back only the row it inserted or the role it added
  (`:13118-13128`); the overwritten e-mail and name stay.
- **Why it matters:** low: the answer (502) says no user was created, yet
  the existing account was renamed.
- **How to check:** `TestCreateUserKeepsAnAdoptedAccountRowOnFailure`
  (`internal/gateway/user_provisioning_test.go`) checks that the row and its
  other roles stay; it does not check the name or e-mail.
- **What closes it:** the old values restored on failure, or the upsert and
  the grant held in a transaction committed only after the invitation is
  sent.

### A failed identity lookup keeps a removed account's sign-in and logs another reason

- **Noticed:** 2026-09-30, keeping a shared identity-provider account when
  one tenant database removes its user (`f0f8986`).
- **What:** with a database per tenant, removing an account
  (`removeUserAccount`, `internal/gateway/handler.go:11934`) keeps the
  identity-provider account while another database still holds the identity
  (`identityHeldElsewhere`, `:11958`). A catalog or control-plane error
  counts as held, so the row goes and the identity-provider account stays;
  the error is dropped, and the log line says another tenant database still
  holds the identity.
- **Why it matters:** low: it never deletes an account wrongly, but leaves
  an identity-provider account that resolves to no user (every request 401)
  and keeps the address taken, with a log line naming the wrong reason.
- **How to check:** read `identityHeldElsewhere`;
  `TestRemovingAnAccountKeepsAnIdentityAnotherDatabaseHolds`
  (`internal/gateway/identity_elsewhere_test.go:20`) covers the held case
  only.
- **What closes it:** the error logged as such, naming the account to
  remove by hand, or the check retried before the row is deleted.

### Removal from a tenant leaves the account's button rules

- **Noticed:** 2026-09-30, adding Remove from this tenant.
- **What:** Remove from this tenant (`removeTenantAccess`,
  `internal/gateway/account_boundaries.go:351`) deletes the account's roles
  in the tenant's workspaces, its business-role memberships, its application
  and model grants there, and its `dimension_member` and `metric` access
  rules on those models. Its `button` access rules stay: their `ref_id`
  cannot be tied to a model or a tenant.
- **Why it matters:** low: a button rule only restricts. If the account is
  added to the tenant again, the old rules apply again.
- **How to check:** `SELECT * FROM identity.user_access_rule WHERE rule_type
  = 'button' AND user_id = …` after a removal.
- **What closes it:** button rules that name their model, so that a removal
  takes them too.

### Removal from a tenant and a refused invitation have no audit event types of their own

- **Noticed:** 2026-09-30, adding existing accounts to a tenant.
- **What:** `pkg/auditlog` has no event type for either. Removal from a
  tenant is recorded as `user.role_revoked` with metadata
  `action=removed_from_tenant` (`adminRemoveFromTenant`,
  `internal/gateway/account_boundaries.go:415`), and a refused invitation of
  an existing account as `user.role_granted` with `granted=false` and
  `refused` naming the reason (`refuseExistingAccount`, `:600`). The refusal
  also carries `visibility=platform`, a new generic marker that `auditScope`
  (`internal/gateway/handler.go:7098`) leaves out for tenant viewers, in
  `GET /api/admin/audit` and the enterprise audit export alike.
- **Why it matters:** an audit reader filtering by event type sees a removal
  as one revoke, and a refusal as a grant.
- **How to check:** `grep -n 'removed_from_tenant\|"granted"'
  internal/gateway/account_boundaries.go`.
- **What closes it:** `user.removed_from_tenant` and a refusal event type in
  `pkg/auditlog`, used here.

### Audit events are written after the change commits

- **Noticed:** 2026-09-30, auditing the developer grants a grant removal
  also revokes.
- **What:** the grants that `removeGrants` and Remove from this tenant
  revoke with an account's last application or model grant are audited after
  the transaction commits, through `auditlog.Log` (`auditRevoked`,
  `internal/gateway/account_boundaries.go:309`), as every other audited
  mutation is.
- **Why it matters:** a crash between the commit and the log loses the audit
  row; the revocation stands.
- **How to check:** `grep -n 'auditlog.Log' internal/gateway/*.go`, each
  against the `tx.Commit` before it.
- **What closes it:** an audit write inside the mutation's transaction (for
  example `auditlog` taking a `pgx.Tx`), adopted route by route.

### No route disables or re-enables an account

- **Noticed:** 2026-09-30, adding the users list's `permissions`. Standing
  rule 2.
- **What:** only SCIM disables an account, and only one the tenant owns
  (`ownedSQL`, `ee/scim/service.go:257`). The users list reports
  `permissions.disable` by the same rule as rename and delete
  (`accountPermissionsFor`, `internal/gateway/account_boundaries.go:108`),
  but no gateway route or screen uses it. Changing an account's e-mail is
  refused (400) for everyone, and the Users screen shows the e-mail
  read-only.
- **Why it matters:** a tenant without SCIM can only delete a member, not
  suspend one.
- **How to check:** look for a disable route in
  `internal/gateway/handler.go`.
- **What closes it:** a decision whether administrators disable accounts; if
  so, a disable and re-enable route refused as `permissions.disable` says,
  and its control on the Users screen shown only when that is true.

### An access_granted notification opens nothing

- **Noticed:** 2026-09-30, adding the notification an added account gets.
- **What:** the `access_granted` notification (`notifyAccessGranted`,
  `internal/gateway/account_boundaries.go:657`) names resource type
  `workspace`, so the notification centre treats it as a link
  (`web/src/ui/NotificationCenter.tsx:35`), but no console section handles
  `workspace` in `onNotificationNavigate`: a click marks it read and closes
  the panel.
- **Why it matters:** low: the message says to sign in and open the
  workspace, which the person then does by hand.
- **How to check:** `grep -rn onNotificationNavigate web/src`.
- **What closes it:** a section that handles `workspace`, for example by
  opening that workspace's first application.

### Mail about another tenant goes out under the platform's name

- **Noticed:** 2026-09-30, sending `access_granted` by e-mail.
- **What:** white-labelled mail takes its brand from the recipient
  (`BrandName`, `cmd/gateway/main.go:364`, through
  `branding.EmailNameForUser`): the account's own tenant, else its first
  workspace's. A message that is another tenant's news, such as tenant B
  telling one of tenant A's accounts that it was added, used to go out under
  A's brand. It now goes out under the platform's name (`OtherTenant`,
  `internal/notification/dispatch.go:321-328`). That also moved workflow
  mail to a member whose home is another tenant from its home brand to the
  platform's.
- **Why it matters:** B's mail would better carry B's brand.
- **How to check:** `internal/notification/dispatch.go:321-328`.
- **What closes it:** `BrandName` taking the message's tenant instead of the
  recipient, so each tenant's mail carries its own brand.

### The Users screen can reopen a removed account in edit mode

- **Noticed:** 2026-09-30, adding Remove from this tenant.
- **What:** when a revoke or a removal from the tenant takes the account
  being edited out of the list, `editId` in
  `web/src/consoles/admin/UsersPanel.tsx` still points at it. Nothing
  happens unless the account is listed again later in the same session: its
  row then reopens in edit mode.
- **Why it matters:** low: a surprise, not a wrong write.
- **How to check:** edit an account of another tenant, remove it from the
  tenant, then invite it again.
- **What closes it:** clearing `editId` when its account leaves the list.

### The invite form cannot be sent while the caller's roles load, and says nothing

- **Noticed:** 2026-09-30, making the invite form ask for a role and a
  workspace (`f0f8986`).
- **What:** until `/api/me` answers, `assignableRoles` is empty, so the
  Users screen's invite form treats the caller as someone other than a
  platform admin (`inviteNeedsRoleAndWorkspace`,
  `web/src/consoles/admin/UsersPanel.tsx:322`) and hides Initial Role,
  which it renders only when `assignableRoles` is not empty (`:372`). The
  Workspace field shows, and Create user stays disabled with no role to pick
  and no reason given.
- **Why it matters:** low: nothing wrong is sent, but on a slow `/api/me`
  the form looks broken.
- **How to check:** slow `/api/me` down in the browser's network tools and
  open the invite form.
- **What closes it:** the form showing that it is loading, or saying why
  Create user is disabled, until the roles arrive.

### The invite e2e spec mocks a refusal the gateway words differently

- **Noticed:** 2026-09-30, making the invite form ask for a role and a
  workspace (`f0f8986`).
- **What:** `web/e2e/account-ownership.spec.ts:92` mocks the 400 of an
  invitation without a role and a workspace as "choose a role and a
  workspace: people are always added to a workspace". The gateway answers
  "role and workspace_id are both required: an invitation gives a role
  inside a workspace, and someone who already has an account is added only
  that way" (`errInviteNeedsRoleAndWorkspace`,
  `internal/gateway/account_boundaries.go:628`). No test ties the two. The
  gateway's side, the same 400 for a new address and an existing account
  before any lookup, is `TestInviteNeedsRoleAndWorkspace`
  (`internal/gateway/admin_tiers_test.go:244`).
- **Why it matters:** low: the spec proves that the screen shows what the
  server says, not what a person would read.
- **How to check:** compare the two strings.
- **What closes it:** the mock using the gateway's wording, or a case in the
  real-Keycloak e2e job.

### The Playwright specs are not type-checked

- **Noticed:** 2026-09-30, editing `web/e2e/account-ownership.spec.ts`.
- **What:** `web/tsconfig.json` references only `tsconfig.app.json`
  (`include: ["src"]`) and `tsconfig.node.json` (`vite.config.ts`), so
  neither `npm run build` (`tsc -b && vite build`) nor CI's `npx tsc -b
  --noEmit` (`.github/workflows/ci.yml:342`) type-checks
  `web/e2e/*.spec.ts`. Playwright strips their types without checking them.
- **Why it matters:** a type error in a spec shows only when the spec runs,
  if at all.
- **How to check:** `grep -n include web/tsconfig.*.json`.
- **What closes it:** a `tsconfig` for `web/e2e` referenced from
  `web/tsconfig.json`, so `tsc -b` checks the specs too.

### Field descriptions and errors are not tied to their controls

- **Noticed:** 2026-09-30, adding the invite form's workspace hint.
- **What:** `Field` (`web/src/ui/Field.tsx:26`) ties its label to the
  control by id, but renders `description` and `error` as plain spans with
  no id, and sets no `aria-describedby` on the control. The invite form's
  "People are always added to a workspace", and every other field's hint or
  error, is not announced with its control.
- **Why it matters:** a screen-reader user misses hints and validation
  errors on every form built on `Field`.
- **How to check:** inspect the invite form's Workspace select: it has no
  `aria-describedby`.
- **What closes it:** `Field` giving the description and the error ids and
  setting `aria-describedby` on the control, as it sets `id`.

### A tenant cannot create a second workspace

- **Noticed:** 2026-09-29, writing the Tenant admin guide.
- **What:** sign-up creates one workspace, "Default"
  (`internal/gateway/signup.go:195`); the API has only `GET
  /api/admin/workspaces` (`internal/gateway/handler.go:517`), and no screen
  creates one. Business roles, the inbox and business-admin scope are per
  workspace, so a tenant cannot separate them.
- **Why it matters:** the engine supports several workspaces a tenant
  cannot reach.
- **How to check:** look for a workspace create route in
  `internal/gateway/handler.go`.
- **What closes it:** a decision whether tenants manage workspaces; if so, a
  create and rename route and screen for the tenant admin.

### Storage use is shown only on Enterprise

- **Noticed:** 2026-09-29, writing the Tenant admin guide.
- **What:** the only screen that shows a tenant's storage is Usage
  (`web/src/ee/usage/UsageTab.tsx:41`, `:76-77`), an Enterprise feature. On
  Community and Commercial, a tenant admin on a plan with a storage limit
  sees nothing until the read-only banner appears.
- **Why it matters:** the first sign of the limit is hitting it.
- **How to check:** on a Community build, look for storage figures as a
  tenant admin.
- **What closes it:** storage use against the limit on a screen every
  edition has.

### Dashboard pages are listed by name

- **Noticed:** 2026-09-29, building the tour.
- **What:** Run › Dashboards orders a model's dashboards by name
  (`internal/gateway/handler.go:14177`, `:14182`). The guides number their
  pages ("1 · Start here", …), so a guide of ten or more pages would list
  "10 · …" before "2 · …". `TestPackagesCarryTheirGuides` checks the
  numbering, not the count.
- **Why it matters:** only a limit on guide length today; any app that
  wants a page order has to encode it in names.
- **How to check:** `grep -n "ORDER BY dd.name" internal/gateway/handler.go`.
- **What closes it:** guides kept under ten pages, or an explicit dashboard
  order a developer can set.

### The Design canvas cuts text widgets sized for Run

- **Noticed:** 2026-09-29, measured live by the tour's reviewer.
- **What:** in Run, a text widget's `size_h` is a minimum
  (`web/src/consoles/business/DashboardWidgets.tsx:73-78`; text is in
  `INTRINSIC_HEIGHT_WIDGET_TYPES`). The Build › Dashboards Design canvas
  draws every widget exactly `size_h` tall, including about 57 px of chrome
  (border, drag header, padded body with `overflow: auto`;
  `web/src/consoles/developer/DashboardCanvas.tsx:796-904`). A text block
  sized to its content for Run shows a scrollbar in Design: each of the
  tour's text blocks hides 28 to 42 px there, and the tour sends readers to
  Design.
- **Why it matters:** Design and Run disagree for every app's text widgets.
- **How to check:** open Design on the tour's "1 · Start here" and compare
  each text body's `scrollHeight` with its `clientHeight`.
- **What closes it:** the canvas drawing a text widget `size_h` plus its
  chrome tall, or the starter's text heights raised at the cost of blank
  space in Run.

### The bundled manuals are not ready for phones, and their screenshots are dated

- **Noticed:** 2026-09-29, bundling the manuals into the web image.
- **What:** the formulas and developer manuals now ship with the console at
  `/docs/…/manual.html` (`web/Dockerfile:41-43`). Neither has a viewport
  meta tag, a dark theme or ids on its headings, so phones render them at
  desktop width and a console link cannot point at a chapter. 32 of the
  developer manual's 33 screenshots were committed on 2026-09-15; some show
  screens that have changed since (`13-account-menu.png` shows a "Developer
  console" heading and a Build group without Roles) and a demo model since
  deleted. Four images are used by neither manual but ship anyway, because
  the Dockerfile copies the whole `img/` directory: `05-grids.png`,
  `10-dependency-graph.png`, `14-notifications.png`, `38-users.png` (about
  300 KB).
- **Why it matters:** the manuals are now one click from every console.
- **How to check:** `grep -c 'name="viewport"' docs/*-manual/manual.html`;
  for each image in `docs/developer-manual/img`, grep its name in both
  manuals.
- **What closes it:** a viewport meta tag and heading ids in the parts
  (`parts/00-head.html` and the `<h2>`s), rebuilt; the screenshots
  recaptured with `docs/developer-manual/build.sh`; the four images used or
  deleted.

### The web image's /docs/ serving is checked only by hand

- **Noticed:** 2026-09-29, bundling the manuals into the web image.
- **What:** `TestPackagesCarryTheirGuides` proves only that the two manuals
  exist in the repository, and the e2e job fetches them through the Vite dev
  server. Nothing runs the built image: `web/nginx.conf`'s `/docs/` handling
  and the Dockerfile's copies are exercised only when `docker-publish-web`
  builds it. That job needs the web jobs only
  (`.github/workflows/ci.yml:735`), not `go-test`, so a dispatch can push an
  image whose manual fails `TestManualsMatchTheirParts` (deploy and release
  do wait for `go-test`). The web nginx sends no Content-Security-Policy;
  the manuals rely on inline `<style>`, so a policy added later needs its own
  rule for `/docs/` (`style-src 'unsafe-inline'`).
- **Why it matters:** a wrong nginx location would ship unnoticed.
- **How to check:** build the image and request
  `/docs/formulas-manual/manual.html` and `/docs/developer-manual/manual.html`.
- **What closes it:** a CI step that runs the built image and expects 200
  for both manuals and 404 for `/docs/nope.html`, and `go-test` among the
  publish job's needs.

### Several documents name console screens by their old labels

- **Noticed:** 2026-09-29, linking the guides to the documentation.
- **What:** `docs/AI_KEYS.md:14,18,40,70` ("Admin › AI keys", "AI Assistant ›
  Settings"), `docs/AUDIT_EXPORT.md:12,50`, `docs/BRAND.md:53`,
  `docs/SSO_SCIM.md:21,67`, `docs/WHITE_LABEL.md:7`, `docs/NOTIFICATIONS.md:27`
  and `docs/USAGE_ANALYTICS.md:7` say "Admin › …". The console's labels are
  Tenant admin › AI keys, Notification delivery, Single sign-on, Provisioning
  (SCIM), Audit Log, Branding and Usage (Platform › … for a platform admin),
  and the personal AI key is under Build › AI Developer › AI Settings.
- **Why it matters:** a reader who follows a guide's link meets different
  names.
- **How to check:** `grep -n 'Admin ›\|AI Assistant ›' docs/*.md`.
- **What closes it:** the current labels, and each file's Last verified date.

### The OpenAPI document leaves out headers, some parameters and most error statuses

- **Noticed:** 2026-09-29, bringing the spec in line with this change.
- **What:** no operation declares `X-App-Id` or `X-Model-Id` (`grep -c "in:
  header" api/openapi.yaml` is 0); model resolution, history narrowing and
  the business-admin workspace depend on them and are described only in
  prose. Several handlers read a `?revision_id=` the spec did not declare;
  this change declared it on the endpoints it touched, but did not audit the
  rest (for example the developer dashboards and form-integrations `POST`).
  Most operations do not list the 401, 403 and 404 the shared helpers
  answer, nor some statuses added on 2026-09-30: 404 from `POST
  /api/admin/users/{id}/roles` and `DELETE /api/admin/users/{id}` for an
  unknown user (the spec lists 200, 400 and 403 on both), and 402 from `POST
  /api/admin/users` at the plan's user cap. (The 409s of an existing address
  and of a last-grant application or model delete, named here before, are
  no longer sent.) `TestRouteSpecParity` compares methods and paths only,
  and nothing checks the operation count in `docs/API.md:133`: it says 251,
  and the spec has 254 operations (`grep -c 'operationId:'
  api/openapi.yaml`, 2026-09-30; 253 before this change added `DELETE
  /api/admin/users/{id}/tenant-access`).
- **Why it matters:** a client generated from the spec cannot send the
  headers the console relies on, or expect the errors it gets.
- **How to check:** grep `internal/gateway/handler.go` for
  `Query().Get("revision_id")` per handler against each operation's
  parameters.
- **What closes it:** the two headers as shared parameters on the operations
  that read them; a `revision_id` audit; a parity test that also compares
  parameters, and a docs test that counts operations.

### The OpenAPI 200 of POST /api/cells does not match the response

- **Noticed:** 2026-09-29, reviewing the cell write.
- **What:** `api/openapi.yaml:3118-3137` declares the 200 body as `{ ok:
  boolean }`; `cells()` answers `{"status":"ok"}`
  (`internal/gateway/handler.go:2136`). The generated type `WritebackOK`
  (`internal/gateway/oas/oas_schemas_gen.go`) has only `Ok`, so a client
  built from the spec reads every success as `ok` absent. The operation
  lists only 400 and 500; it also answers 401 (no actor), 402 (plan
  limit) and 403 (model or revision outside the caller's scope, or the
  write guard refusing), as the entry above says of most operations.
- **Why it matters:** the write every grid makes is described wrongly.
  Nothing in the tree uses the generated client, so nothing breaks today.
- **How to check:** compare the two places above.
- **What closes it:** the spec naming `status` and the 401, 402 and 403
  responses, and the ogen code regenerated.

### The web client builds member trees in three places

- **Noticed:** 2026-09-29, making every member list follow `sort_order`.
- **What:** `buildMemberTree` in `web/src/consoles/dashboardLayout.ts:51`
  (context selectors, `HierarchicalMemberSelect`), `buildMemberTree` in
  `web/src/consoles/business/PlanningGrid.tsx:129` (grid axes) and
  `buildDimensionTree` in `web/src/consoles/developer/DimensionsTab.tsx:15`
  (the Build tree) are near-duplicates; this change had to remove the code
  sort from each one separately.
- **Why it matters:** an ordering change made in one drifts from the others.
- **How to check:** `grep -rn "function buildMemberTree\|function
  buildDimensionTree" web/src`.
- **What closes it:** one shared builder.

### Reduced test schemas keep drifting from the migrations

- **Noticed:** 2026-09-29, while giving workflow assignment one predicate.
- **What:** `internal/workflow`, `internal/notification`,
  `internal/aiassistant`, `internal/query`, `internal/crudapp`,
  `internal/identity`, `internal/importpkg` and others test against
  hand-written schemas in their `testdata/`. Each column a query starts to
  read needs a matching edit there: this change added
  `internal/workflow/testdata/014_assignee_boundary.sql` and
  `internal/gateway/testdata/003_default_model.sql` for that reason. The
  assignment predicate's own tests run on the real migrations
  (`internal/workflow/assignee/assigneetest`).
- **Why it matters:** a reduced schema can pass a query that fails against
  the real one, or fail one that works.
- **How to check:** `ls internal/*/testdata/*.sql`.
- **What closes it:** those packages' test setup moved onto the real
  migrations (`testdb` with `migrationfs`).

### business_role_member has no index on user_id

- **Noticed:** 2026-09-29, while giving workflow assignment one predicate.
- **What:** `identity.business_role_member`'s only index is its primary key,
  role then user (`migrations/020_business_roles_access.sql:19-23`). Every
  per-user business-role check (inbox, eligibility, notification recipients,
  reminders) filters on `user_id`, and resolving a role's recipients
  evaluates the check once for every user.
- **Why it matters:** slow on large tenants; not measured.
- **How to check:** `EXPLAIN` the recipient query of
  `resolveNotificationRecipients` on a large database.
- **What closes it:** an index on `user_id`, in a new migration.

### Loose ends in the getting-started guides

- **Noticed:** 2026-09-29, reviewing the guides.
- **What:**
  - The tour's one-number picture prints 48,000 where the grid shows
    $48,000 (`oneNumber`, `internal/starter/diagrams.go:142`);
    `internal/starter/starter_test.go` looks for the bare number.
  - The Developer guide says "People, business roles and access rules
    belong to the whole workspace, not to a revision"
    (`internal/starter/developer.go:227`). Business roles are per
    workspace, but access rules are per user and name a model's members and
    metrics: "not to a revision" holds, "the whole workspace" is loose.
  - Picture widgets' sizes are typed by hand beside their SVG; no test
    compares a widget's `SizeW`/`SizeH` with its SVG `viewBox` (only the
    one-number picture returns its own height).
  - `kpiHeight` (164) rests on a rendered height of 161 px, typed into
    `internal/starter/kpi_height_test.go:10` from a live measurement; a
    change to the KPI tile's CSS would not be noticed.
- **Why it matters:** small inaccuracies in the first thing a new tenant
  reads.
- **How to check:** as listed.
- **What closes it:** the currency sign in `oneNumber` and the test
  accepting it; a precise sentence; a starter test comparing each picture's
  aspect ratio with its `viewBox` (within about 2%); an e2e check of a
  titled KPI's rendered height.

### An AI member add that creates its parent can pass the member limit by one

- **Noticed:** 2026-09-29, re-checking the AI Developer's plan-limit hooks.
- **What:** `add_dimension_member` checks the plan for one new member
  (`internal/aiassistant/write_executor.go:1011`). When its parent code names
  no member of the same dimension, it then creates that parent
  (`:1113-1118`) and inserts the member (`:1129`): two rows for a check of
  one. Seen by reading; not yet exercised live.
- **Why it matters:** a dimension one member below
  `max_members_per_dimension` can end one above it. The next add is refused,
  so the overshoot stays at one.
- **How to check:** on a plan with a member limit, fill a dimension to one
  below it, ask the assistant to add a member under a parent code that does
  not exist yet, and count the members.
- **What closes it:** a `checkMembers` for the parent before it is created,
  with a case in `TestPlanLimitHooks`.

### A docs-only edit runs the whole Go gate

- **Noticed:** 2026-09-29.
- **What:** the CI change filter (`.github/workflows/ci.yml:91`) now sets
  `go=true` for any path under `docs/` and any `.md` file, because Go tests
  read the documents (the three tests of `docs_test.go`, and
  `TestFormulasManualIndexMatchesEngine` in
  `internal/formula/catalogue_test.go:774`). So every document edit, this
  file included, runs every job gated on `go`, Go test (about nine minutes)
  among them. The e2e suite runs only when a manual changes (`:98`).
- **Why it matters:** CI time for each observation or document edit. The
  trade-off was made on purpose and is recorded nowhere else.
- **How to check:** the change-detection step (`ci.yml:83-103`); a push that
  touches only `docs/OBSERVATIONS.md` starts Go test.
- **What closes it:** a docs-only change running only the tests that read
  the documents (`go test -run 'TestDocs|TestManuals' .` and
  `go test -run TestFormulasManualIndexMatchesEngine ./internal/formula`),
  or a decision that the full gate is wanted.

### A form widget Playwright test failed once on a cold Vite start

- **Noticed:** 2026-09-29, running five spec files with three workers; the
  failure was not kept, so the failing step is not recorded here.
- **What:** on the first run after a cold `npm run dev`, one "dashboard
  form widget" test (`web/e2e/form-permissions.spec.ts:85` or
  `web/e2e/form-record-permissions.spec.ts:171`) failed and passed on the
  rerun. Locally Playwright runs with no retries and the default worker
  count; CI runs one worker with two retries (`web/playwright.config.ts:7-8`),
  which would hide it. A guess, not checked: Vite compiling the console on
  first request while three workers load it.
- **Why it matters:** a local run can report a failure that is not one.
- **How to check:** stop Vite, clear `web/node_modules/.vite`, and run the
  five specs with `--workers=3`.
- **What closes it:** a reproduction that names the step; if it is the
  cold compile, a warm-up (a global setup that loads the console once)
  before the tests.

### The audit export and workflow history read one database

- **Noticed:** 2026-10-01, making the platform admin's views span every
  database (`docs/TENANT_DATABASES.md`, "What a platform admin sees").
- **What:** with a database per tenant, the audit log list now merges every
  database's newest events for a platform admin, but the enterprise audit
  export (`internal/gateway/audit_export.go`) streams the database the
  request is routed to. `GET /api/workflow/my-history` and the business
  admin console's workflow history read the routed home only, and a step
  completed from that history is completed there; only the inbox
  (`GET /api/tasks`) spans a person's homes and addresses each task's own.
  `GET /api/admin/applications`, which the console does not call, lists the
  routed database's applications only.
- **Why it matters:** an export or a history taken from one home looks
  complete and is not.
- **How to check:** in dedicated mode, export the audit log as a platform
  admin with no tenant chosen, and compare with the list; open the workflow
  history of a person in two tenants.
- **What closes it:** the same per-database merge as the lists
  (`homesOf`, `platformHomes`), and a tenant address on history rows.

### Stand-ins are no workflow assignees

- **Noticed:** 2026-10-01, letting platform-wide builders work in dedicated
  tenants through stand-ins (`internal/gateway/stand_in.go`).
- **What:** the assignee predicate (`internal/workflow/assignee`) reads roles
  from the database the step is in. A platform admin's or platform-wide
  builder's stand-in holds none, so in a dedicated tenant they are assignees
  of nothing — not of a step naming `developer`, which in the control plane
  a platform-wide builder is.
- **Why it matters:** the same workflow routes a step differently in a
  shared and in a dedicated tenant.
- **How to check:** a workflow in a dedicated tenant with a step naming
  `developer`; the platform-wide builder's inbox.
- **What closes it:** a decision whether platform-level accounts should be
  assignees in tenants at all; stand-ins are never mailed.

### Per-tenant settings follow a tenant admin's first tenant

- **Noticed:** 2026-10-01, making lists span a person's homes.
- **What:** a tenant admin of two dedicated tenants now sees both tenants'
  people, applications and audit in one list. The settings tabs (SSO, SCIM,
  notifications, branding, AI keys) have a tenant picker for platform admins
  only; a tenant admin's requests there go to their oldest membership, so the
  second tenant's settings are reached only by opening one of its
  applications first.
- **Why it matters:** standing rule 2, for an administrator of more than one
  tenant.
- **How to check:** in dedicated mode, a person who is tenant admin of two
  dedicated tenants opens the SSO tab.
- **What closes it:** the settings tabs' tenant picker offered to anyone who
  administers more than one tenant, listing those tenants.

### Usage for a tenant admin covers the routed tenant only

- **Noticed:** 2026-10-01, reviewing the per-home lists.
- **What:** the users, workspaces, audit and Applications lists span every
  tenant a tenant admin administers, in every database that holds them, but
  `GET /api/admin/usage` reports the one tenant the request is routed to
  (`currentCustomerID`, `internal/gateway/usage.go`). A tenant admin of two
  tenants sees the second's usage only after opening one of its
  applications.
- **Why it matters:** the Usage tab silently shows one tenant of several.
- **How to check:** in dedicated mode, a tenant admin of two dedicated
  tenants opens Usage.
- **What closes it:** usage per home where the person holds tenant_admin,
  merged as the other lists are.

### Switching to a model with no revision strands the business consoles

- **Noticed:** 2026-10-01, testing the Dashboards model selector.
- **What:** a model made through Admin › Applications (`POST /api/admin/models`)
  has no revision until a developer creates one. Selecting it — from the
  Dashboards selector or the Models tab — makes `/api/demo` answer 500
  ("no revision found for model"). The business_user section then shows
  "Cannot reach gateway" on Dashboards, and the business-admin section shows an
  empty page; neither renders the model selector, since it needs the context.
- **Why it matters:** a business user who picks a model a developer has not
  started yet sees a misleading connection error, and has to know to go to the
  Models tab to switch back.
- **How to check:** as a tenant admin, create a model in an application a
  business user reaches; as that user, pick it on Dashboards.
- **What closes it:** list only models with a revision in `/api/apps` for
  business routes, or have `/api/demo` report "no revision yet" as its own
  state that the consoles render with the selector still shown.

### The gateway test package is outgrowing CI's time limits

- **Noticed:** 2026-10-02, when a deploy's Go test job failed: `go test -race
  -p 4 ./...` killed `internal/gateway` at Go's default 10-minute package
  timeout, and the deploy was skipped.
- **What:** the package's CI time in green runs went 339s (09-29), 377s,
  382s, 405s, 467s, 423s, 437s (10-01), then past 600s. No one test is slow —
  locally the whole package takes about 150s under `-race` (363 tests, the
  slowest the 18s 10k-row scale test) — but the self-hosted runners run it
  roughly three times slower, four packages at once, each starting its own
  Postgres container, and it grows with every feature's tests. CI now passes
  `-timeout 20m`, inside the job's 30-minute limit.
- **Why it matters:** at this rate the package reaches the new limit within
  weeks, and every failure blocks a deploy until the tests are re-run.
- **How to check:** `gh run view <run> --log | grep -E 'ok\s+.*internal/gateway\s'`
  over recent green runs of `ci.yml`, against 1200s.
- **What closes it:** cutting the runtime rather than raising limits — one
  shared Postgres with per-test schema isolation (the CI step's own comment
  says it would remove most of it), or splitting `internal/gateway`'s tests
  so they spread over the four parallel slots.

### The .xlsx upload reads cells as displayed, so formatted numbers are refused

- **Noticed:** 2026-10-02, same change.
- **What:** `importpkg.ParseXLSXRows` (`POST /api/import/upload` with
  `xlsx_base64`, used by the business Import widget) reads each cell as Excel
  displays it. A value formatted `#,##0.00` arrives as `"1,234.50"`, which
  `ResolveRows` refuses as not a number, and the whole workbook is rejected.
  `importpkg.ParseTabularFile` (the AI Developer's attachment import) reads
  stored values instead; the upload endpoint was left as it was because a
  date-formatted cell used as a member code would change from its displayed
  text to Excel's serial number.
- **Why it matters:** finance workbooks nearly always format numbers with
  thousands separators, so a business user's upload of one fails with a
  validation error on every value row.
- **How to check:** upload a workbook whose value cells use number format 4
  through the business Import widget; the response is 422 with
  `"1,234.50"`-style raw values. `TestParseTabularFileReadsStoredValuesAndPicksSheet`
  shows the two readings side by side.
- **What closes it:** reading stored values for value columns (or all
  columns, after deciding what a date-typed member-code cell should become),
  then `ParseXLSXRows` can call `parseXLSXSheet(data, "", true)`.

### Dimension member imports are not held to the plan's member limit

- **Noticed:** 2026-10-02, same change.
- **What:** `importDimensionMembersCSV` (`POST /api/import/dimension-members`
  and a `csv_import` integration run with a dimension target) adds members
  without `plan.Enforcer.CheckMembers`, which the member endpoints and the AI
  Developer's member tools run. The AI Developer's attachment import counts
  the new members first and checks them (`newMemberCount` in
  `internal/gateway/ai_file_import.go`); the developer's own imports do not.
- **Why it matters:** on a plan with a member limit, an import passes it by
  any number of members.
- **How to check:** on a customer whose plan limits members, import a CSV with
  more new codes than the remaining allowance through the Import Wizard — it
  succeeds.
- **What closes it:** counting the file's new codes in
  `importDimensionMembersCSV` (as `newMemberCount` does) and calling
  `CheckMembers` before the first insert, for every caller.

## Closed

### A saved Excel/CSV integration could not be run from a dashboard with a normal file

- **Noticed:** 2026-10-02, while adding Excel/CSV integrations to the AI Developer.
- **What it was:** `POST /api/integrations/{id}/run` for a `csv_import` grid
  integration took CSV text only and needed a `metric_id` column of metric
  **UUIDs** plus `value`: it staged the raw cell as the metric id, checked no
  member, and ignored the saved `import_mode`. The console's "Run Import"
  worked only because the Import Wizard resolves names to ids in the browser;
  a business user's dashboard button (`IntegrationButtonWidget`) sent the file
  as read with `readAsText`, so an `.xlsx` could not be sent, and a CSV of
  metric names — the files the wizard and the AI Developer map — failed with
  "grid csv must have 'metric_id' and 'value' columns". Google Sheets syncs
  went through `ResolveRows` but skipped the plan's limits and kept no run
  history.
- **Closed:** 2026-10-02 (`e7f8810`), in the change that found it (AI file
  import and data exports). Every file integration's grid run shares
  `runGridIntegration` (`internal/gateway/integration_file_run.go`): the run
  takes `csv` or `xlsx_base64` (+ `sheet`, stored cell values), applies the
  saved column map in the wizard's vocabulary (`importpkg.ApplyColumnMap`),
  resolves names and leaf members, commits valid rows through the write guard
  and the plan's fact and storage limits, lists the invalid ones, honours the
  saved `import_mode`, recalculates and records the run. The dashboard button
  sends a workbook as base64 and shows the first failing row. With the
  owner's agreement the July salary demo's non-negative rule went from
  `ResolveRows`: negative values import on every path, as typed cells and the
  REST connector already allowed. Proof: `TestBusinessUserRunsANameMappedFileIntegration`,
  `TestDimensionIntegrationRunsFromAWorkbook`, `TestImportRejectsABadRowAtomically`
  and the dashboard-button cases in `web/e2e/ai-file-integration.spec.ts`,
  run 2026-10-02: pass.

### A first sign-in could be aimed at another tenant

- **Noticed:** 2026-10-01, reviewing the routing rewrite; the behaviour
  predates it.
- **What it was:** tenant routing treated a subject with no directory
  membership as platform-level and honoured its `X-Tenant-Id` for any
  dedicated tenant. A first sign-in through tenant X's identity provider
  has no membership yet: aimed at tenant B, it was served in B's database
  with the account X's first-login provisioning had just made, roles and
  all. With X's default role `tenant_admin`, it passed B's administrator
  gates — B's SSO settings and SCIM tokens — which take the tenant from the
  routed database.
- **Closed:** 2026-10-01 (`57d565d`). Platform level comes from platform
  reach in the control plane only (`tenantRouting`,
  `internal/gateway/tenant.go`); a subject held nowhere stays on the control
  plane. An account a first sign-in makes is never served in another
  tenant's database (`resolveJWTActor`). Proof:
  `TestRoutingDecidesByMembershipAndReach`, run 2026-10-01: pass; it fails
  with an empty membership counted as platform level.

### A dedicated tenant's SCIM could change a platform admin's sign-in

- **Noticed:** 2026-10-01, reviewing the per-home changes.
- **What it was:** someone the control plane holds with platform reach can
  also be a dedicated tenant's own person — a platform admin who signed up a
  tenant, or the control plane's later adoption of a tenant's person. That
  tenant's database said nothing of the platform role, so its SCIM token
  owned the row and could change the shared sign-in address (a
  password-reset takeover), disable or delete it, and its administrators
  could rename, re-invite and delete it.
- **Closed:** 2026-10-01 (`57d565d`). Such a row is a platform account's
  to the tenant (`platformReachOf`, `internal/gateway/stand_in.go`): SCIM
  does not own it (`PlatformReach`, `ee/scim/service.go`), the users
  routes refuse it to anyone but a platform admin, and an invitation does
  not add to it. Proof: `TestScimDoesNotAdoptSomeoneAnotherDatabaseHolds`
  (Dana), run 2026-10-01: pass; it fails with the control plane's answer
  ignored.

### The platform admin did not see every database

- **Noticed:** 2026-10-01, mapping the platform console in dedicated mode.
- **What it was:** with a database per tenant, the platform admin's users,
  workspaces and audit log read one database — the control plane, or one
  tenant chosen at a time. Usage statistics left out every tenant still in
  the control plane, skipped a tenant whose database was not ready, and
  stopped on one that failed to open. A dedicated tenant whose database could
  not be read was listed with no applications and no reason. The application
  last opened (`X-App-Id`, sent on every request) took the platform admin's
  tenants list, users, audit and infrastructure views into that tenant's
  database, where the control plane's tenants vanished. A platform admin's
  preferences and personal AI key followed the tenant being worked on, and
  creating a dedicated tenant's application lost its audit event to a
  foreign key.
- **Closed:** 2026-10-01 (`57d565d`). Every list reads every database for
  a platform admin (`platformHomes`, `internal/gateway/tenant.go:392`), each
  row naming its database (`tenant_id`): users (`usersIn`,
  `internal/gateway/handler.go:6987`), workspaces (`workspacesIn`, `:7335`),
  tenants (`tenantsIn`, `:6650`), applications (`appsIn`, `:9879`), the audit
  log (`auditEventsIn`, `:7183`), and usage (`internal/gateway/usage.go`),
  which also lists a tenant it could not count with why, as the tenants list
  does. A platform-level caller's `/api/admin/` requests ignore `X-App-Id`;
  `X-Tenant-Id: control-plane` names the control plane. Preferences and AI
  settings of an actor resolved from the control plane stay there
  (`personalCtx`); infrastructure nodes are read there. The application
  event is written in the tenant's database under the actor's stand-in. The
  console's Users tab is one list; **People of** narrows it, and each action
  is addressed to its row's database. Proof:
  `TestPlatformAdminSeesEveryDatabase`
  (`internal/gateway/homes_test.go`) and
  `web/e2e/platform-dedicated-users.spec.ts`, run 2026-10-01: pass; with the
  per-database listing switched off the first fails.

### A person in several databases reached only one, and one list leaked another's applications

- **Noticed:** 2026-10-01, mapping how people in more than one database are
  served.
- **What it was:** the control plane was never a destination for someone
  with a dedicated membership, so a person of a shared-database tenant who
  was also in a dedicated tenant could not reach the shared tenant at all.
  Lists read the routed database only: a member of a second dedicated tenant
  never saw its applications, tasks or notifications. And the
  administration's tenants list appended the person's other dedicated
  tenants and listed their applications by the routed database's roles: a
  tenant admin of one tenant who was a business user of another saw all of
  the other's applications, models and revisions.
- **Closed:** 2026-10-01 (`57d565d`). The control plane is addressed by
  one of its tenants or applications, or as `control-plane`, for a caller it
  holds (`tenantRouting`, `internal/gateway/tenant.go`). Lists run once per
  home with the person's own account there and merge (`homesOf`, `:351`):
  applications, the Applications lists, the inbox and notifications (and
  marking them read), users, workspaces, audit. A home lists only by what the
  person holds in it; each row names its home, and the console addresses a
  task's completion to it. Invitations add someone another database holds as
  a member, wherever the invitation is made. Proof:
  `TestAPersonReachesEveryHome` (`internal/gateway/homes_test.go`), run
  2026-10-01: pass; it fails with the per-home listing, the control-plane
  address or the per-home role check switched off.
- **Found in review before commit, and closed with it:** a home where the
  person's account is deactivated was still their default route, so one
  tenant deactivating its row (by SCIM) answered 401 to every request they
  made elsewhere; routing now counts only homes with an active account
  (`activeHomes`, `internal/gateway/tenant.go`). Completing a task listed
  for a platform admin's own account in a tenant ran as their stand-in and
  was refused; it runs as that account (`taskAction`; read, not run). A
  shared-database tenant's SCIM could deactivate or delete a member row
  the other shared tenants rely on; only a dedicated tenant's SCIM does
  (`OwnDatabase`, `ee/scim/service.go`). Deleting a dedicated tenant's
  application from the platform console, and the developer list's revision
  and default-model actions on another tenant's row, went to the wrong
  database; each is addressed to its row's tenant. Proof:
  `TestRoutingDecidesByMembershipAndReach`, run 2026-10-01: pass; it fails
  with deactivated homes counted.

### A platform-wide builder did not reach dedicated tenants

- **Noticed:** 2026-09-30, resolving platform admins from the control plane.
- **What it was:** only a platform admin was resolved from the control plane
  when addressing a dedicated tenant; a platform-wide builder was looked up in
  the tenant's database, had no account there, and was refused, and the
  tenant list offered it only its own memberships.
- **Closed:** 2026-10-01 (`57d565d`). `platformActorOnControl` resolves a
  platform-wide builder too, with a stand-in (`internal/gateway/stand_in.go`);
  `isGlobalBuilder` answers from what the control plane said
  (`actor.onControl`); the Applications lists read every database for it
  (`platformHomes`). Proof:
  `TestAPlatformWideBuilderReachesDedicatedTenants`
  (`internal/gateway/homes_test.go`): it builds in Acme, and once narrowed by
  an application grant it is refused; run 2026-10-01: pass.
- **Found in review before commit, and closed with it:** the builder kept
  every role it held in the control plane, so a tenant_admin of a shared
  tenant passed a dedicated tenant's administrator gates — SSO, SCIM
  tokens, branding, usage. There it is only a developer now
  (`platformActorOnControl`). Proof: the same test, refused on each of those
  routes; it fails with the roles kept.

### A control-plane account that adopted a dedicated tenant's person was left unused

- **Noticed:** 2026-09-30, reviewing the start-up clean-up.
- **What it was:** a control-plane creation that had adopted a dedicated
  tenant's person was left alone, and — since the control plane was never
  addressed for someone with a directory entry — unused, including a
  platform role a platform admin had given it.
- **Closed:** 2026-10-01 (`57d565d`). The control plane is reachable for
  such a person (above). A platform role there makes them platform-level:
  routed as a platform admin or builder even though a tenant holds them. A
  row a shared-database tenant owned becomes a member's there, with a role it
  held without a workspace moved into that tenant's workspaces
  (`ReconcileAdoptedAccounts`, `internal/gateway/reconcile_adoptions.go`).
  Proof: `TestReconcileAdoptedAccounts` (Ed's and Dana's cases), run
  2026-10-01: pass.

### A platform_admin role already in a tenant's database was shown but granted nothing

- **Noticed:** 2026-09-30, refusing platform-level grants in tenant
  databases.
- **Closed:** 2026-10-01 (`57d565d`). The start-up clean-up removes every
  `platform_admin` grant a tenant's database holds, audited for the platform
  only (`ReconcileAdoptedAccounts`). Proof: `TestReconcileAdoptedAccounts`
  (Ann's planted grant), run 2026-10-01: pass.

### A platform admin could not act on a dedicated tenant

- **Noticed:** 2026-09-30, testing invitations with a database per tenant.
- **What it was:** a platform admin's request addressed to a dedicated
  tenant (`X-Tenant-Id`, or `X-App-Id` of its application) ran in the
  tenant's database, where the platform admin has no account: every one
  answered 401. The platform console's Users tab showed and invited the
  control plane's people only, and tenant creation names no first
  administrator, so a tenant the platform admin created in dedicated mode
  could not be given its people from the console (standing rule 2). The
  settings tabs' tenant picker, and a tenant's model creation, revisions
  and export, failed the same way.
- **Closed:** 2026-09-30 (`57d565d`). A request addressed to a tenant by
  someone with no tenant of their own is marked (`tenantRouting`,
  `internal/gateway/tenant.go`), and its actor is read from the control
  plane; only a platform admin is resolved that way
  (`platformActorOnControl`, `internal/gateway/stand_in.go:49`), and
  anyone else resolves as before. The tenant's database holds a stand-in
  under the platform admin's id, so that what they write there names them
  (`ensureStandIn`, `:91`; migration 103): their name and a reserved
  address, no tenant, no role or grant (a trigger refuses them), left out
  of the users list and the dev persona list, never an actor by itself, and
  answered 404 by the users routes. The platform Users tab has a **People
  of** picker when tenants with their own database exist, and then
  addresses every call to the chosen one (`UsersScopePicker`,
  `web/src/consoles/platform-admin/section.tsx:114`; `tenantId`,
  `web/src/consoles/admin/UsersPanel.tsx`); the tenant list marks those
  tenants `dedicated`. Proof: `TestPlatformAdminActsOnADedicatedTenant`
  (`internal/gateway/platform_stand_in_test.go`: the invitation, its
  `assigned_by` and audit event naming the platform admin, the stand-in
  unlisted and 404 to the tenant's admin, the trigger, workspaces,
  notification settings, audit, a model's creation, revision and export,
  and a demoted platform admin refused), and
  `web/e2e/platform-dedicated-users.spec.ts`, run 2026-09-30: pass; with the
  control-plane resolution switched off the test answers 401 again.
- **Found in review before commit, and closed with it:** acting in a
  tenant's database, a platform admin could grant `platform_admin` there,
  which the actor lookup then trusted: an administrator of the platform
  the control plane could not see or demote. A tenant's database now
  refuses `platform_admin`, and `developer` or `tenant_admin` without a
  workspace on an account with no tenant, which would be platform-wide
  there (`dedicatedGrantErr`, `internal/gateway/stand_in.go:150`); a
  `platform_admin` found in a tenant's database grants nothing
  (`actorByKeycloakSub`, `internal/gateway/handler.go:934`); the Users tab
  does not offer it for a dedicated tenant. And any writer could take a
  stand-in's reserved address first; migration 103 reserves the domain
  (`stand_in_address_reserved`), and invitations and SCIM refuse it with
  400. Proof: `TestPlatformAdminActsOnADedicatedTenant`, run 2026-09-30:
  pass; with either rule switched off it fails.

### A platform admin whose e-mail a tenant's account used could not act on that tenant

- **Noticed:** 2026-09-30, adding platform admins' stand-ins.
- **What it was:** a platform admin's stand-in in a dedicated tenant's
  database carried the platform admin's address, which is unique per
  database. An account of the tenant's under that address kept the stand-in
  from being written, and the platform admin's requests to that tenant
  answered 401. Every tenant a platform admin acted in also stored their
  address.
- **Closed:** 2026-09-30 (`57d565d`). A stand-in carries a reserved
  address built from the platform admin's id, `<id>@stand-in.invalid`,
  which is never mailed (`standInEmail`, `internal/gateway/stand_in.go:82`).
  Nothing uses a stand-in's address: it has no role, so no notification
  reaches it, and the audit log shows the actor's name. Proof:
  `TestAStandInDoesNotNeedThePlatformAdminsAddress` and the address check in
  `TestPlatformAdminActsOnADedicatedTenant`
  (`internal/gateway/platform_stand_in_test.go`), run 2026-09-30: pass; with
  the platform admin's own address the first answers 401.

### A member added from another database was shown as having no tenant

- **Noticed:** 2026-09-30, adding someone another dedicated tenant's
  database holds as a member (`addMemberFromElsewhere`).
- **What it was:** the member's row belongs to no tenant, because its
  tenant is in another database, and the users list said `home_tenant:
  none`, which the console shows as not a member of any organisation.
  Remove from this tenant left the directory entry, so the tenant stayed
  among the person's tenants, empty. The name and e-mail were copied once,
  when the member was added.
- **Closed:** 2026-09-30 (`57d565d`). The list labels such a row
  `other` — "Member of another organisation" — when another database, a
  dedicated tenant's or the control plane, holds the person
  (`labelMembersFromElsewhere`,
  `internal/gateway/account_boundaries.go:273`). Remove from this tenant
  forgets the directory entry in a dedicated tenant's database, and adding
  the person again restores it (`:595`, `addExistingAccount`). The row
  follows the name and e-mail the person signs in with, and only such a
  row does (`followIdentity`, `:746`). Proof:
  `TestAnInvitationDoesNotAdoptSomeoneAnotherDatabaseHolds`,
  `TestAMemberFollowsTheirSignIn`
  (`internal/gateway/identity_elsewhere_test.go`) and
  `TestReconcileAdoptedAccounts`
  (`internal/gateway/platform_stand_in_test.go`), run 2026-09-30: pass;
  without the label the first reports `none`.

### An existing account invited without a workspace is told it was invited, and nothing happens

- **Noticed:** 2026-09-30, adding existing accounts to a tenant. Standing
  rule 2.
- **What it was:** an invitation of an address that already had an account
  added it only with a role inside a workspace. With no role, or no
  workspace, it answered as a new invitation does (200, `status: created`),
  granted nothing, sent nothing, and recorded the refusal in an audit event
  only platform admins see. The Users screen let a tenant admin invite with
  role None, and with `developer` or `tenant_admin` and "No specific
  workspace", and then confirmed the invitation. The same invitation of a
  new address made an account, so the users list afterwards told the two
  apart as well.
- **Closed:** 2026-09-30 (`f0f8986`), by the user's decision that anyone
  but a platform admin invites with a role inside a workspace. `POST
  /api/admin/users` refuses such a request with 400 before the address is
  looked at, alike for a new address and an existing account
  (`internal/gateway/handler.go:12908`, `errInviteNeedsRoleAndWorkspace`,
  `internal/gateway/account_boundaries.go:628`). The invite form asks for
  both whenever the caller is not a platform admin ("Select a role…",
  "Select a workspace…", the hint "People are always added to a
  workspace"), and Create user stays disabled until both are chosen
  (`inviteNeedsRoleAndWorkspace`,
  `web/src/consoles/admin/UsersPanel.tsx:322`). A platform admin still
  invites with no role or no workspace. Proof:
  `TestInviteNeedsRoleAndWorkspace`
  (`internal/gateway/admin_tiers_test.go:244`), run 2026-09-30: pass;
  `web/e2e/account-ownership.spec.ts` (the invite cases at `:362`, `:403`
  and `:431`), run 2026-09-30: pass. Split out: "The invite form cannot be
  sent while the caller's roles load, and says nothing", "The invite e2e
  spec mocks a refusal the gateway words differently", "The Playwright
  specs are not type-checked" and "Field descriptions and errors are not
  tied to their controls" (Open, above).
- **Behaviour change:** a tenant admin's or developer's `POST
  /api/admin/users` with no role, or no workspace, answers 400. It used to
  create a new address's account with no role, or with `developer` or
  `tenant_admin` for the whole tenant; such a grant is now a second step,
  on the account's roles route.

### A platform admin's invitation made an account with no tenant

- **Noticed:** 2026-09-30, closing account adoption at first sign-in; split
  from "Nothing sets an existing account's tenant" (Open, above).
- **What it was:** `POST /api/admin/users` left a new account's
  `customer_id` empty when the creator's scope was every tenant (a platform
  admin, or a platform-wide builder), even when it named the workspace the
  role went into. Such an account belonged to no tenant: its tenant's
  administrators could not rename, delete or re-invite it, and its tenant's
  single sign-on refused its first sign-in once its identity-provider
  subject no longer matched.
- **Closed:** 2026-09-30 (`f0f8986`), by the user's decision that an
  account belongs to the tenant of what it holds. A new account invited
  into a workspace belongs to that workspace's tenant, whoever invites it
  (`internal/gateway/handler.go:12916-12931`); a platform admin's creation
  with no workspace belongs to none, and an account a platform admin's
  creation adopts keeps its own (`:13071-13089`). `user.created` records
  `workspace_id` and `customer_id` (`:13136`). Migration 102
  (`migrations/102_account_tenant_backfill.sql`) gives an account with no
  tenant the one tenant its workspace roles, business-role memberships and
  application and model grants lie in, and leaves alone a platform admin,
  an account holding `developer` or `tenant_admin` without a workspace, one
  the directory lists in a tenant database, one whose holdings lie in
  several tenants or none, and every account in a tenant's own database.
  Proof: `TestPlatformAdminInviteBelongsToTheWorkspacesTenant` and
  `TestPlatformAdminInviteKeepsAnExistingAccountsTenant`
  (`internal/gateway/admin_tiers_test.go`),
  `TestPlatformWideBuilderKeepsItsInvitees`
  (`internal/gateway/account_boundaries_test.go`),
  `TestBackfill102AccountTenant` and
  `TestBackfill102LeavesTenantDatabasesAlone`
  (`migrations/backfill_102_test.go`), run 2026-09-30: pass. Split out:
  "Nothing sets an existing account's tenant" (narrowed), "Migration 102
  adds accounts to a tenant's user count", "Migration 102 writes no audit
  event", "Migration 102 tells a tenant database by its name" and "A
  platform admin's failed re-invitation keeps the new name and e-mail"
  (Open, above).
- **Behaviour change:** flagged as scope growth: the rule applies to every
  creator, not only a platform admin, for consistency with 102, which
  backfills the same shape whoever made it. A platform-wide builder's
  invitee now belongs to the workspace's tenant (`home_tenant` `own`, where
  it was `none`), and the builder keeps every action on it. A tenant's
  administrators now rename, delete and re-invite the accounts a platform
  admin invited into their workspaces, and those 102 gives them. Reverting
  the growth means an empty `customer_id` again for a platform-wide
  builder's invitations.

### A tenant admin cannot add a person who already has an account

- **Noticed:** 2026-09-30, closing account adoption on user creation.
  Standing rule 2.
- **What it was:** `POST /api/admin/users` answered 409 when the application
  already had an account at the address, or under the identity-provider
  subject it resolved to, unless the caller was a platform admin
  (`existingAccountAllowed`). A person who already had an account in another
  tenant, or one with no tenant, could be added to a tenant's workspace only
  by a platform admin.
- **Closed:** 2026-09-30 (`9f689db`), by the user's decision that a tenant
  admin may add such a person. Inviting the address
  (`internal/gateway/handler.go:12954-12970`, and `:12996-13009` for an
  address the identity provider knows under another account's subject) goes
  to `addExistingAccount` (`internal/gateway/account_boundaries.go:545`): it
  adds the requested role inside the requested workspace of the caller's
  tenant, checked as a new invitation's role is, and nothing else. The
  account's name, e-mail, tenant, active flag and identity-provider account
  stay as they are, and no invitation is re-sent. The person is told in the
  notification centre, and by e-mail when that workspace's tenant sends
  notifications by e-mail (`notifyAccessGranted`, `:657`; at most once a day
  per workspace and three times per tenant). The answer is the one a new
  invitation gets. What cannot be added is answered the same, under an id
  that names no account, grants nothing, and is audited for the platform
  only (`refuseExistingAccount`, `:600`): a request with no role or no
  workspace; a platform admin's or a platform-wide builder's account; and an
  account with no tenant that holds `tenant_admin` without a workspace,
  which any role in a new tenant's workspace would make that tenant's
  administrator (`customerlessAdminGainsTenant`, `:629`). The users list
  shows such an account with `home_tenant` `other` or `none` and only what
  it holds in the caller's tenants, and Remove from this tenant (`DELETE
  /api/admin/users/{id}/tenant-access`, `adminRemoveFromTenant`, `:415`)
  takes that back: its roles in the tenant's workspaces, its business-role
  memberships, its application and model grants there, and its member and
  metric access rules on those models, and nothing else of the account.
  Proof: `TestInviteExistingAccountAddsItToTheTenant`,
  `TestRemoveAccountFromTenant`, `TestUsersListShowsOnlyTheTenantsPart` and
  `TestNoRoleMakesACustomerlessAdminAnotherTenantsAdmin`
  (`internal/gateway/account_boundaries_test.go`), run 2026-09-30: pass;
  `web/e2e/account-ownership.spec.ts`, run 2026-09-30: pass. Split out: "An
  existing account invited without a workspace is told it was invited, and
  nothing happens", "Inviting an existing address still shows afterwards
  that it had an account", "The plan's user cap refuses adding an account
  that is already a member", "Removal from a tenant leaves the account's
  button rules", "Removal from a tenant and a refused invitation have no
  audit event types of their own", "An access_granted notification opens
  nothing" and "Mail about another tenant goes out under the platform's
  name" (Open, above; the first closed 2026-09-30, above).
- **Behaviour change:** `POST /api/admin/users` no longer answers 409 for an
  address that has an account: a tenant admin's invitation adds a workspace
  role to it. For a caller who is not a platform admin, `POST` and `DELETE
  /api/admin/users/{id}/roles` of a role without a workspace on an account
  whose home is elsewhere answer 403 (`internal/gateway/handler.go:13241`,
  `:13309`); workspace roles and application and model grants inside the
  caller's tenant stay allowed on such an account, since inviting it again
  reaches the same.

### An invitation cannot be resent from the console

- **Noticed:** 2026-09-29, writing the Tenant admin guide. Standing rule 2.
- **What it was:** `POST /api/admin/users/{id}/invite` existed, but nothing
  in `web/src` called it. An invitation expires after 72 hours
  (`inviteLifetime`); after that, the only way left in the console was to
  delete the person and invite them again.
- **Closed:** 2026-09-30 (`9f689db`). The Users screen has a Resend
  invitation action (`reinviteUser`,
  `web/src/consoles/admin/UsersPanel.tsx:242`; `resendAdminUserInvite`,
  `web/src/api/client.ts:1965`) on each account the users list's
  `permissions.reinvite` allows, which is the rule the route itself applies
  (`accountActionAllowed`, `internal/gateway/handler.go:13146`). Proof:
  "Resend invitation confirms where it went"
  (`web/e2e/account-ownership.spec.ts:254`), run 2026-09-30: pass.

### Re-sending a record's current status might count as a status change

- **Noticed:** 2026-09-29, while restricting who may move a form record.
- **What it was:** a question, checked either way: whether a client that
  sends a record's current status with a field edit is judged as moving the
  record, and fires its rules again.
- **Closed:** 2026-09-29 (`6ef9219`), nothing to fix. Neither console
  re-sends the current status: Run › Forms sends a status only when the
  user picks a different one (`web/src/consoles/business/FormsTab.tsx:184`),
  and the dashboard widget's status select cannot fire for the value it
  already shows. The server treats the current status as no move
  (`RecordAccess.CanUpdate`, `internal/crudapp/permissions.go:84-87`: `to
  == from` needs only edit rights), and the rules fire only on a change
  (`statusChanged`, `internal/gateway/handler.go:10563`).
  `TestFormRecordResendingCurrentStatusIsNoChange`
  (`internal/gateway/form_list_permissions_test.go:99`) guards it for API
  clients.

### Dimension members could not be reordered

- **Noticed:** 2026-09-29, when grid axes and selectors started following
  `sort_order`. A gap under standing rule 3, raised.
- **What it was:** no endpoint, screen or AI tool set or changed a member's
  `sort_order`; a member added later could only go last.
- **Closed:** 2026-09-29 (`6ef9219`), by the user's decision to build it
  for the developer role only. `PUT
  /api/developer/dimensions/{id}/members/order` (the `dev` guard; another
  tenant's or an unknown dimension answers 404) and the AI Developer's
  `reorder_dimension_members` both run `modeledit.ReorderMembers`
  (`internal/modeledit/reorder.go`). It takes one level: the children of a
  parent, which may sit in the dimension or outside it, in the wanted order.
  It refuses a time dimension, and names the wrong members in a 400. It
  renumbers the whole dimension in tree order in one transaction, and waits
  for a concurrent re-parent. Each reorder writes a
  `dimension.members_reordered` audit event. Build › Dimensions has move up
  and down controls (`web/src/consoles/developer/DimensionsTab.tsx`).
  Revision duplication and model export/import carry `sort_order`. Proof:
  `internal/gateway/member_reorder_test.go` (route, roles, scope parity with
  the other member edits, parent on either side, concurrent re-parent,
  revision copy, export/import) and `TestReorderDimensionMembers`
  (`internal/aiassistant/reorder_members_test.go`). Still open, split out:
  "A member added or re-parented after a reorder lists after the last
  subtree" and "The gRPC member create writes sort_order 0 and the old
  parent column".

### The grid and the legacy cell write preferred a dimension named "department"

- **Noticed:** 2026-09-29. Standing rule 1.
- **What it was:** `grid()` ordered a model's dimensions `ORDER BY
  (d.name='department') DESC, d.name, …` when no grid definition was named,
  and `cells()` resolved the legacy single `dim_code` field against the
  dimension named `department` first. A demo's dimension name was wired into
  the engine: a model with a dimension called `department` got a different
  axis order, and a different target for a legacy cell write, from one that
  called it anything else.
- **Closed:** 2026-09-29 (`6ef9219`), by the user's decision to remove it.
  Both grid branches order dimensions by name only (then members by time
  index, sort order, code). The legacy `dim_code` goes through
  `resolveLegacyDimCode` (`internal/gateway/legacy_dim_code.go`): the one
  dimension of the revision that has a member with that code, among the
  metric's own grid dimensions when it has any, else the whole model; no
  match or several matches answer 400 and ask for `dim_codes`. Proof:
  `TestLegacyDimCodeAndGridOrderIgnoreDimensionNames`
  (`internal/gateway/legacy_dim_code_test.go`), which fails on the old code.
  `grep -rn "'department'" internal/gateway/handler.go` finds nothing.

### The AI Developer's writes skipped the plan limits

- **Noticed:** 2026-09-28, giving the AI `update_dimension`; kept in
  `docs/OBSERVATIONS_PRIVATE.md` while it was open.
- **What it was:** the developer endpoints check the tenant's plan before
  adding rows (`plan.Enforcer.CheckMembers`, `CheckMetrics`, …), but the AI
  Developer's write tools (`create_dimension` members and derive,
  `add_dimension_member`, `update_dimension` derive, `create_metric`, …)
  called no plan check, so a tenant on a limited plan could exceed
  `max_members_per_dimension` or `max_metrics_per_model` by asking the
  assistant.
- **Closed:** 2026-09-29 in `8ede7fe` (released). The `WriteExecutor` takes
  `CheckMetrics` and `CheckMembers` hooks
  (`internal/aiassistant/write_executor.go:69-100`), which the gateway wires
  to the plan enforcer (`internal/gateway/ai_handler.go:1693-1705`), and
  calls them before each insert (`write_executor.go:489`, `:797`, `:931`,
  `:1011`, `write_executor_dimension.go:232`, `write_executor_edit.go:183`).
  Proof: `TestPlanLimitHooks` (`internal/aiassistant/edit_tools_test.go`).
  Re-checked 2026-09-29. One residue: when `add_dimension_member` creates a
  missing parent (`write_executor.go:1113`), the check counted one row for
  two, so a dimension can end one member over the limit; open as "An AI
  member add that creates its parent can pass the member limit by one".

### The debug facts and calc views showed hidden members to a developer

- **Noticed:** 2026-09-28, final review of the dimensional-references change;
  kept in `docs/OBSERVATIONS_PRIVATE.md` while it was open. The gap predates
  that change; the entry "A restricted user reads an old revision
  unrestricted" (below) claimed the debug views enforced the rules.
- **What it was:** `GET /api/developer/debug/calc?dim_members=` listed every
  persisted row at a combination, filtering only metric rules: rows at a
  hidden member's own combinations, totals above it, and values the grid
  withholds (`LOOKUP(revenue, region, "US")` at UK). `GET
  /api/developer/debug/facts` lists facts of every revision but filtered them
  by the requested revision's hidden members, whose dimension IDs a fact of
  another revision never carries. Reachable by a person holding the
  developer role and a hidden rule.
- **Closed:** 2026-09-28 in `8c04baa` (released). The per-combination calc
  view answers 403 to a caller with any hidden member (persisted rows cannot
  be told apart from values derived from hidden members); the facts view
  filters each fact by its own revision's rules. Proof:
  `TestDebugViewsHonourDeveloperHiddenMembers` (`internal/gateway`), which
  fails with either fix switched off.

### A restricted user reads an old revision unrestricted

- **Noticed:** 2026-09-28, by the stage 3 verifier; kept in
  `docs/OBSERVATIONS_PRIVATE.md` while it was open.
- **What it was:** `GET /api/grid?revision_id=<old>` served a non-active
  revision unrestricted. Activation re-points every `dimension_member` and
  `metric` rule at the new revision's rows
  (`remapAccessRulesToRevision`), so the old revision's members and metrics
  matched no rule. Grid export, chart-data, `/api/metrics` and cell writes
  into the old revision had the same gap.
- **Closed:** 2026-09-28 in `8c04baa` (released), by the decision and fix
  below.
- **Decision:** rules are resolved by LINEAGE against whatever revision is
  read. Every dimension, member and metric has a `lineage_id` it shares with
  its copies in every revision of its model; a rule records the lineage of
  the row it was written on (`ref_lineage_id`). A `dimension_member` rule
  applies to the member of that lineage in the requested revision, a
  `metric` rule to the metric of that lineage. Restricted users keep reading
  old revisions, with the same things hidden or read-only, whatever has been
  renamed since.
- **Fix:** one resolver, `writeguard.RulesForRevision` / `RuleMaps`
  (`internal/writeguard/rules.go`). It translates each rule's `ref_id` into
  the requested revision by lineage; revision-less dimensions and metrics
  count as part of every revision. A rule whose lineage has no row there is
  dropped.
  When two rules land on one row, the stricter access wins. `button` rules
  pass through unchanged, and errors fail closed. The single-id checks
  (`HiddenAccess`, `MetricAccess`, and through them `HiddenInChain` and
  `CheckWrite`/`CheckWriteMetrics`) match by the same lineage.
  Every consumer that reads rules now uses the resolver:
  - the grid (`loadUserAccessRules`, every branch including `totals_only`,
    `sliceFast` and `fastTotals`);
  - grid export and the debug facts/calc views (`hiddenMemberFilter`);
  - chart-data (`ChartResolver.loadAccessRules`, whose row-scan errors now
    fail closed);
  - `/api/metrics`, `/api/dimensions`, the developer dimension list and the
    business-admin pickers;
  - the form-record filter (resolved against the form's revision);
  - the full-reload import delete (`importpkg.restrictedIDs`, metrics
    included);
  - the gRPC `Query` (`visibleDimCodes`, now scoped to the requested
    revision; an "everything hidden" result no longer reads as "no
    restriction");
  - workflow-context redaction (now `HiddenInChain`, failing closed).
- **Proof:** `TestRestrictedUserReadsOldRevisionRestricted` and
  `TestHiddenMetricOnOldRevision` (`internal/gateway`) reproduce the entry
  over HTTP: hide US, make DE read-only, hide a metric, then create and
  activate a revision. They check the old revision's grid (with and without
  a grid), `totals_only`, export, chart-data, `/api/metrics` and cell writes,
  and the active revision alongside. With the resolver switched off, every
  old-revision assertion fails. `TestRulesForRevisionResolvesByLineage`
  (`internal/writeguard`) covers the dropped target, stricter-wins, the
  revision-less dimension, a malformed `ref_id`, a rule stored without a
  lineage and the pass-through rule type.
- **Renames and deletes (closed in the same change):** matching by name
  alone was reopened by two routine developer edits, both found by the
  stage 4 verifier over HTTP. Renaming the dimension, a member code or a
  metric in the active revision left the older revisions' copies under the
  old name, so they matched no rule again — reads served US and cost, and a
  write to the read-only DE in the old revision returned 200. Deleting the
  member a rule pointed at left the rule's `ref_id` dangling with nothing to
  match from. A first fix (a rename log, identity columns captured on the
  rule, SECURITY DEFINER triggers) was replaced before release by lineage
  ids, the design decided on 2026-09-28. Migration
  `099_access_rule_identity.sql` adds `lineage_id` (NOT NULL, default
  `gen_random_uuid()`, indexed) to `model.dimension_def`,
  `model.dimension_member` and `model.metric_def`, backfilled so rows that
  share an identity across a model's revisions share one lineage
  (dimensions by `lower(name)`, members by (`lower(dimension name)`, code),
  metrics by `lower(name)`, revision-less rows included), and
  `identity.user_access_rule.ref_lineage_id`, backfilled from the row
  `ref_id` points at (NULL for `button`). No triggers: application code
  keeps both.
  - Every definition copy carries lineage: the developer's revision create
    (`handler.go`), the AI `create_revision`, and model export/import
    (`internal/modeltransfer`: export writes `lineage_id`; import keeps the
    package's, and mints fresh ones — consistently, one per package lineage
    — when it is missing or already taken in this database, i.e. a re-import
    next to its source; the sign-up starter goes through the same import).
    New rows (member POST, CSV / connector upserts, AI adds) get a fresh
    lineage; an upsert of an existing (dimension, code) keeps its own.
  - Every rule write sets `ref_lineage_id` from the referenced row.
    `writeguard.ReplaceUserRules`, used by the business-admin
    `PUT .../access-rules`, updates kept rules in place and keeps a rule's
    lineage when its row is gone, so a re-save of the listed rules (the UI
    re-sends every listed `ref_id`) does not lose it. The AI
    `set_user_access_rules` names members by (dimension, code) in the active
    revision only, so it cannot name a rule whose member is gone from that
    revision, nor any metric or button rule; it uses
    `writeguard.ReplaceUserMemberRulesInRevision`, which replaces only the
    user's member rules that resolve (by lineage) in the active revision and
    keeps every other rule. Before this, an AI member-rule edit deleted the
    rule on a deleted (or deleted and re-added) member — un-hiding the old
    revisions' copy — and every metric rule of the user in every revision;
    `list_users` now also shows metric rules and counts the kept member
    rules (and it works again: it had failed on every call, since
    `string_agg` over the `identity.user_role` enum does not exist — the
    new test was its first caller). `remapAccessRulesToRevision` re-points
    `ref_id` at the active revision's row of the rule's lineage, for the
    admin's listing only — enforcement never depends on `ref_id`.
  - Semantics: a rename never changes lineage, so it never matters. A member
    or metric deleted and re-added is a NEW lineage: unrestricted in the
    active revision until an admin sets a rule on it, while the old
    revisions keep the original restricted.
  - Proof: `TestAccessRulesSurviveRenames`,
    `TestAccessRulesFollowRenameBeforeActivation` and
    `TestAccessRulesAfterDeleteAndReAdd` (`internal/gateway`, HTTP),
    `TestRevisionDuplicationCopiesLineage` and `TestModelExportImportLineage`
    (`internal/gateway`), `TestCreateRevision_CopiesLineage`
    (`internal/aiassistant`),
    `TestSetUserAccessRules_KeepsRulesItCannotName` (`internal/aiassistant`:
    delete + re-add in the active revision, then an AI re-affirm keeps the
    old revision's member and a metric rule hidden),
    `TestReplaceUserMemberRulesInRevisionScope` (`internal/writeguard`),
    `TestRuleLineageSurvivesRenameAndDelete`
    (`internal/writeguard`) and `TestBackfill099Lineage` (`migrations`,
    applies 001–098, seeds, applies 099).

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
- **Closed:** 2026-09-29 (`8ede7fe`), by the user's decision: a metric used
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
- **Closed:** 2026-09-29 (`8ede7fe`). Checked read-only on 2026-09-29:
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
- **Closed:** 2026-09-29 (`8ede7fe`). Migration `101_dimension_name_case.sql`
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
- **Closed (the race):** 2026-09-29 (`8ede7fe`). Both name triggers
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
- **Closed:** 2026-09-29 (`8ede7fe`), by decision to follow Excel.
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
- **Closed:** 2026-09-29 (`8ede7fe`). The scheduler recovers at its entry
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
- **Closed:** 2026-09-29 (`8ede7fe`). The Add metric form uses the row
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
- **Closed:** 2026-09-28 (`8ede7fe` — the AI Developer parity audit). The
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
- **Server side:** closed 2026-09-28 (`8c04baa`, the flat-parent change):
  `rollup.Resolve` leaves a missing leaf out of an average. The grid client
  (`resolveCell`) is what remains.
- **Closed:** 2026-09-28 (`8c04baa` — the flat-parent change): the grid
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
- **Closed:** 2026-09-28 (`8c04baa` — the flat-parent change): the grid
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
- **Closed:** 2026-09-28 (`8c04baa` — the flat-parent change):
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
- **Closed:** 2026-09-28 (`8c04baa` — this change). The 400 descriptions of
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
- **Closed:** 2026-09-28 (`8c04baa` — this change). The AI Developer has an
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
- **Closed:** 2026-09-28 (`8c04baa` — this change).
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
- **Closed:** 2026-09-28 (`8c04baa` — this change), with the product
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
- **Closed:** 2026-09-28 (`8c04baa` — this change). `api/openapi.yaml` now
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
- **Closed:** 2026-09-28 (`8c04baa` — this change), with "The OpenAPI
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
- **Closed:** 2026-09-28 (`8c04baa` — this change), with "The OpenAPI
  document does not list the 409 of a dimension or property delete" above.

### A duplicate member code answers 500 with the SQL error

- **Noticed:** 2026-09-28, final review (a throwaway HTTP test). It was never
  recorded as open.
- **What:** `POST /api/developer/dimensions/{id}/members {"code":"EMEA"}`, when
  EMEA exists, and a member `PATCH` that recodes a member to a taken code,
  answered 500 with the text of `dimension_member_dimension_id_code_key`. The
  AI's `add_dimension_member` wrapped the same violation as a generic
  "insert dimension member" error.
- **Closed:** 2026-09-28 (`8c04baa`, final-review fix round).
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
- **Closed:** 2026-09-28 (`8c04baa`, final-review fix round).
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
- **Closed:** 2026-09-28 (`8c04baa`, this change): `POST`/`PATCH
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
- **Closed:** 2026-09-28 (`8c04baa`, the `MEMBER_IN_USE` change): the
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
- **Closed:** 2026-09-28 (`8c04baa`, this change): the chart's leaf branch
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
- **Closed:** 2026-09-28 (`8c04baa`, this change): the scalar path and the
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
- **Closed:** 2026-09-28 (`8c04baa`, this change), before that design was
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
- **Closed:** Closed 2026-09-28 (`8c04baa`, this change): `update_dimension_property` and `delete_dimension_property` mirror the developer PATCH/DELETE — same validator, value migration on rename, draft- and model-scoped, audited.

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
- **Closed:** Closed 2026-09-28 (`8c04baa`, this change): a bare dimension name that is not pinned now reads blank in every evaluator (`formula.EvalContext.unboundDimension`); an `agg_rule = formula` total that tests the member computes instead of failing.

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
- **Closed:** Closed 2026-09-28 (`8c04baa`, this change): replaced by `TIME_OFFSET_NOT_INTEGER` (decimal literal at save, non-whole dynamic offset at run time) and `MOVING_WINDOW_NOT_LITERAL`; the time-series spec §9 records the replacement for API clients.

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
