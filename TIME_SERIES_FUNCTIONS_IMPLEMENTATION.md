# Time-Series Formula Functions — Implementation Instructions

> **Classification:** Current — Phase 1 implemented; contract and acceptance
> reference.
>
> **Status:** Implemented 2026-09-19 (Phase 1, plus time hierarchies — see
> the note after the deviations). Every acceptance value in
> §11 is asserted by `internal/formula/time_test.go` (evaluator) and
> `internal/calculation/timeseries_integration_test.go` (persisted through
> the real scheduler); the API and schema rules of §12 by
> `internal/gateway/time_dimension_api_test.go`; the Sales Planning demo now
> reads its prior quarter with `LAG`/`PREVIOUS` on a declared time dimension.
> §13 remains the list of functions that are still rejected.
>
> Implementation map: migration `087_time_dimensions.sql`; `internal/timedim`
> (period validation, chronological `time_index`); `internal/formula/time.go`
> + `analyze.go` (functions, `TimeEvalContext`, dependency windows);
> `internal/metricformula` (`Validate`, `Plan`/temporal SCCs,
> `ValidateTime`/`ValidateGridTime`); `internal/calculation/timeseries.go`
> (per-period evaluation, recurrences, time summaries);
> `rollup.ResolveTime` (read paths); `DimensionsTab.tsx` / `MetricsTab.tsx`.
>
> Two rules were settled during implementation and differ from a literal
> reading of §3.3/§3.4: (1) a `CUMULATE` reset expression is read at every
> period of the run (it decides where each run starts), so it is recorded as
> an unbounded-past dependency, not `[0, 0]`; (2) `time_summary` reduces time
> for combining rules (`sum`/`average`/`count`) and for every time-series
> formula, but a plain `formula`/`rate` metric keeps totalling as its formula
> evaluated at the aggregate — across time as across any other dimension —
> so a margin percentage on a time grid is still total margin over total
> revenue, never a sum of quarterly percentages.
>
> **Time hierarchies (beyond Phase 1 as written):** a time dimension may be
> a hierarchy (Q1 → H1 → FY26). A member WITH dates is a leaf period — the
> axis time functions move along, contiguity and boundary rules apply to
> leaves only, `time_index` is dense over leaves. A member WITHOUT dates is
> an aggregate period: a grouping (no ordinal), which may only have
> aggregates as ancestors, may not carry dates once it has children, and
> whose value on every read path is its leaf descendants reduced by the
> metric's `time_summary` (formula/rate rules re-evaluate at the aggregate
> with each operand reduced by its own summary). Time functions never run
> at an aggregate. Enforced by `timedim.ValidateHierarchy`; persisted rows
> `{H1}`, `{FY26}` are written by the scheduler; proven by
> `TestTimeHierarchyAggregatePeriods` and the Sales Planning demo's
> `FY26 › H1/H2 › Q1..Q4` period dimension. Migration `088_time_hierarchy.sql`
> lifts 087's flat-only constraint.
>
> **Last verified against the repository:** 2026-09-19.
>
> **Primary code areas:** `internal/formula`, `internal/metricformula`,
> `internal/calculation`, `internal/gateway`, `internal/modeltransfer`,
> `web/src/consoles/developer`, `api/openapi.yaml`, and `proto/model/v1`.

## 1. Required outcome

Mavericks must support time-series formulas over an explicitly declared time
dimension. A dimension is a time dimension only when the developer marks it as
such when creating it. Names such as `month`, `year`, or `period` must never
implicitly make an ordinary dimension a time dimension.

The first production release must support these case-insensitive functions:

```text
PREVIOUS, NEXT, LAG, LEAD, OFFSET, MOVINGSUM,
CUMULATE, DECUMULATE, MONTHTODATE, QUARTERTODATE, YEARTODATE
```

The essential product contract is:

1. `dimension_type = "time"` is explicit and immutable after creation.
2. Time members have validated dates and one deterministic chronological order.
3. A metric may have at most one time dimension.
4. A time-series formula runs once per leaf time period while preserving every
   non-time coordinate, such as product, department, entity, and version.
5. An unmarked dimension is never used as time, even if its members look like
   dates.
6. Invalid time context is rejected when the model is validated or published;
   it must not become a run-time zero.
7. Grid, chart, export, workflow, API, and persisted calculation values must all
   use the same server result.

This is Anaplan-like compatibility, not a claim of complete Anaplan language
parity. Section 13 lists functions that must remain rejected until their full
semantics are implemented.

## 2. Why scalar built-ins are insufficient

The current evaluator resolves a formula from a scalar `Vars` map. The current
scheduler builds that map for one dimensional combination and then calls
`EvaluateWithDims`. That works for `revenue - cost`, but not for
`LAG(revenue, 1, 0)`: the evaluator must resolve `revenue` at another time
coordinate while keeping all other coordinates fixed.

Do not implement `LAG` or `MOVINGSUM` as ordinary eager functions in
`internal/formula/functions.go`. A correct implementation needs:

- the current time coordinate;
- the ordered period set;
- a callback that evaluates an expression at another period;
- range evaluation for moving and cumulative functions;
- time-aware dependency metadata; and
- a scheduler that can evaluate causal cross-period recurrences.

The existing operational `PartitionMonth(time.Now())` value in
`internal/calculation/scheduler.go` is unrelated to modeled time. It is a
runtime partition key and must not be used to determine the current formula
period.

## 3. Persisted model contract

Create `migrations/087_time_dimensions.sql` unless a lower-numbered migration
has already claimed that number when implementation starts.

### 3.1 Dimension fields

Add these columns to `model.dimension_def`:

```sql
ALTER TABLE model.dimension_def
  ADD COLUMN dimension_type TEXT NOT NULL DEFAULT 'standard',
  ADD COLUMN time_granularity TEXT,
  ADD COLUMN fiscal_year_start_month SMALLINT;

ALTER TABLE model.dimension_def
  ADD CONSTRAINT dimension_def_type_ck
    CHECK (dimension_type IN ('standard', 'time')),
  ADD CONSTRAINT dimension_def_time_granularity_ck
    CHECK (time_granularity IS NULL OR time_granularity IN
      ('day', 'week', 'month', 'quarter', 'half_year', 'year', 'custom')),
  ADD CONSTRAINT dimension_def_fiscal_month_ck
    CHECK (fiscal_year_start_month IS NULL OR
           fiscal_year_start_month BETWEEN 1 AND 12),
  ADD CONSTRAINT dimension_def_time_config_ck
    CHECK (
      (dimension_type = 'standard' AND time_granularity IS NULL AND
       fiscal_year_start_month IS NULL)
      OR
      (dimension_type = 'time' AND time_granularity IS NOT NULL AND
       fiscal_year_start_month IS NOT NULL)
    );
```

Rules:

- Existing dimensions backfill as `standard` and retain existing behavior.
- New HTTP and gRPC requests default omitted `dimension_type` to `standard` for
  backward compatibility, but the Developer Console must require an explicit
  Standard/Time choice.
- `dimension_type`, `time_granularity`, and `fiscal_year_start_month` are
  immutable. To correct a wrongly typed dimension, create a replacement and
  migrate the model. Do not allow an in-use dimension to change semantic type.
- A time dimension cannot have `parent_dimension_id` or
  `source_dimension_id` in Phase 1.
- A grid may contain no more than one time dimension.

### 3.2 Time-member fields

Add these columns to `model.dimension_member`:

```sql
ALTER TABLE model.dimension_member
  ADD COLUMN period_start DATE,
  ADD COLUMN period_end DATE,
  ADD COLUMN time_index INT;

ALTER TABLE model.dimension_member
  ADD CONSTRAINT dimension_member_period_ck
    CHECK (
      (period_start IS NULL AND period_end IS NULL AND time_index IS NULL)
      OR
      (period_start IS NOT NULL AND period_end IS NOT NULL AND
       time_index IS NOT NULL AND time_index >= 0 AND
       period_start <= period_end)
    );

CREATE UNIQUE INDEX dimension_member_time_index_uq
  ON model.dimension_member (dimension_id, time_index)
  WHERE time_index IS NOT NULL;

CREATE UNIQUE INDEX dimension_member_period_start_uq
  ON model.dimension_member (dimension_id, period_start)
  WHERE period_start IS NOT NULL;
```

Application validation, preferably reinforced by a deferred constraint trigger,
must enforce:

- standard-dimension members have all three time fields `NULL`;
- time-dimension members have all three fields populated;
- periods in one dimension do not overlap;
- `time_index` is dense from zero and follows `period_start` order;
- `parent_member_id` is `NULL` for time members in Phase 1 (superseded: see
  the time-hierarchies note in the status block); and
- regular granularities have valid boundaries and no gaps. `custom` may have
  unequal durations but still cannot overlap.

The server, not the caller, owns `time_index`. On insert, update, or delete it
must reindex all members in one transaction by `(period_start, period_end,
code)`. Facts remain safe because fact coordinates use member codes rather than
the ordinal.

Time functions move by positions in this ordered member list. They never sort
by label, code, UUID, or the existing generic `sort_order`.

### 3.3 Time summary method

Add an axis-specific time summary to `model.metric_def`:

```sql
ALTER TABLE model.metric_def
  ADD COLUMN time_summary TEXT NOT NULL DEFAULT 'sum',
  ADD CONSTRAINT metric_def_time_summary_ck
    CHECK (time_summary IN
      ('sum', 'average', 'min', 'max', 'first', 'last', 'none'));
```

`agg_rule` continues to describe non-time dimensional aggregation.
`time_summary` describes aggregation across time. This distinction is required
for balance metrics: flow values normally use `sum`, while closing balances
normally use `last`.

Never evaluate a time-relative formula at the aggregate `{}` coordinate; there
is no current period there. Calculate leaf periods first, then apply
`time_summary`. For a total spanning time and other dimensions, reduce non-time
dimensions first using `agg_rule`, then reduce time using `time_summary`. Add
golden tests for this order.

### 3.4 Dependency metadata

Extend `model.calc_dependency` so validation and scheduling know how a
dependency is used:

```sql
ALTER TABLE model.calc_dependency
  ADD COLUMN min_time_offset INT NOT NULL DEFAULT 0,
  ADD COLUMN max_time_offset INT NOT NULL DEFAULT 0,
  ADD COLUMN unbounded_past BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN unbounded_future BOOLEAN NOT NULL DEFAULT false;
```

Offsets are source positions relative to the result period. Direct references
are `[0, 0]`; `PREVIOUS(x)` is `[-1, -1]`; `LEAD(x, 2, 0)` is `[2, 2]`;
`MOVINGSUM(x, -2, 0)` is `[-2, 0]`. When one formula references the same metric
more than once, store the union of all call-site ranges in its single existing
dependency row.

`CUMULATE(x)` has `unbounded_past = true` and `max_time_offset = 0`.
References in a substitute or reset expression are direct `[0, 0]`
dependencies because those arguments are evaluated in the result period.

## 4. Developer API and console

### 4.1 Create a time dimension

Extend `CreateDimensionRequest`, the manual gateway request, OpenAPI, protobuf,
generated clients, and the AI Developer tool schema:

```json
POST /api/developer/dimensions
{
  "name": "month",
  "revision_id": "<revision-uuid>",
  "dimension_type": "time",
  "time_granularity": "month",
  "fiscal_year_start_month": 1
}
```

The Developer Console's New dimension form must contain:

- **Dimension type:** Standard or Time;
- **Granularity:** visible and required for Time;
- **Fiscal year starts:** visible and required for Time.

Display a `time · month` badge on time-dimension cards. Do not infer Time after
the developer enters a name.

### 4.2 Add time members

Extend member create/update requests and imports:

```json
POST /api/developer/dimensions/{dimensionId}/members
{
  "code": "2026-01",
  "label": "Jan 2026",
  "period_start": "2026-01-01",
  "period_end": "2026-01-31"
}
```

For a time dimension, replace the parent picker with start and end date fields.
Show chronological order, not alphanumeric code order. Add a bulk period
generator with `start`, `end`, and granularity only after the single-member API
is correct; the generator must call the same validation service.

CSV dimension-member import must recognize `period_start` and `period_end`.
Export/import and revision copy must preserve all new dimension, member, metric
summary, and dependency fields.

Every dimension response used by the Developer Console or Business Console must
include `dimension_type`, `time_granularity`, and
`fiscal_year_start_month`. Every time-member response must include
`period_start`, `period_end`, and read-only `time_index`. Update the manual
gateway structs (`devDimension`, `gridDimension`, and their member structs),
OpenAPI schemas, protobuf messages, TypeScript client types, grid/export
queries, and AI read tools together.

### 4.3 Metric time summary

Extend metric create/update requests and `MetricDef` responses with
`time_summary`. In `MetricsTab.tsx`, show a **Time summary** selector only when
the metric belongs to a time-dimensioned grid. Explain `sum` for flows,
`last` for closing balances, `first` for opening balances, and `none` when a
time total is meaningless.

Changing `time_summary` must invalidate and recalculate the metric's aggregate
rows without changing its leaf-period values.

### 4.4 Model validation

Formula save performs syntax and reference analysis. Grid configuration and
revision publication perform dimensional validation because this repository
currently derives a metric's dimensions from `grid_metric` joined to
`grid_dimension`.

A revision cannot publish when any time-series metric:

- has no time dimension;
- has more than one time dimension;
- references a series with a different time dimension;
- uses an empty or invalid time dimension;
- uses a time function at a non-leaf time aggregate; or
- participates in a non-causal temporal dependency cycle.

Longer term, metric dimensionality should become an explicit model contract
rather than an accidental property of grid placement. That refactor is not
required to ship Phase 1, but time validation must be repeated whenever a grid
adds or removes a metric or dimension.

## 5. Phase 1 formula contract

All names and keywords are case-insensitive. Phase 1 values persisted in
`runtime.calc_result` remain numeric. Boolean expressions are allowed where a
function explicitly requests one, such as the reset argument to `CUMULATE`.

Offset arguments must be integer literals in Phase 1. Reject decimals,
references, and calculated offset expressions with
`DYNAMIC_TIME_OFFSET_UNSUPPORTED`. This restriction makes dependency ranges
and recurrence direction statically knowable.

### 5.1 Position functions

| Function | Source position at result period `t` | Boundary result |
|---|---:|---|
| `PREVIOUS(value)` | `t - 1` | numeric zero |
| `NEXT(value)` | `t + 1` | numeric zero |
| `LAG(value, n, substitute)` | `t - n` | `substitute` |
| `LEAD(value, n, substitute)` | `t + n` | `substitute` |
| `OFFSET(value, n, substitute)` | `t + n` | `substitute` |

`value` is an expression, not only a metric identifier. Evaluate the whole
expression at the selected period while preserving all non-time coordinates.
Evaluate `substitute` at the current result period, never at the missing source
period.

`LAG` and `LEAD` also accept a fourth keyword:

```text
LAG(value, n, substitute, NONSTRICT)
LAG(value, n, substitute, SEMISTRICT)
LAG(value, n, substitute, STRICT)
```

- `NONSTRICT` is the default and accepts positive, zero, and negative `n`.
- `SEMISTRICT` returns the substitute when `n < 0`.
- `STRICT` returns the substitute when `n <= 0`.

The same sign rule applies to `LEAD`. `OFFSET` is equivalent to non-strict
`LEAD`. The optional Anaplan list/dimension argument is not part of Phase 1;
the declared time dimension is always the axis.

### 5.2 Moving range

Supported syntax:

```text
MOVINGSUM(source)
MOVINGSUM(source, start)
MOVINGSUM(source, start, end)
MOVINGSUM(source, start, end, method)
```

Mavericks adopts the modern Anaplan/Polaris two-argument behavior to remove the
Classic/Polaris ambiguity:

- one argument: aggregate the entire configured time range;
- two arguments: aggregate from `t + start` through the last period;
- three or four arguments: aggregate the inclusive range from `t + start`
  through `t + end`;
- periods outside the configured range are ignored; and
- `start > end` returns numeric zero.

Phase 1 methods are the bare keywords `SUM`, `AVERAGE`, `MIN`, and `MAX`.
Default is `SUM`. The parser may continue representing them as identifiers,
but semantic reference extraction must recognize the fourth argument as a
keyword rather than looking for a metric named `SUM`.

Examples:

```text
MOVINGSUM(sales, -2, 0)          // trailing three-period sum
MOVINGSUM(sales, -2, 0, AVERAGE)
MOVINGSUM(forecast, 1, 3, MAX)   // next three periods
```

### 5.3 Cumulative and difference functions

```text
CUMULATE(source)
CUMULATE(source, reset_boolean)
DECUMULATE(source)
```

- `CUMULATE(source)` sums from the first configured period through the current
  period, inclusive.
- When `reset_boolean` is true at a period, that period starts a new cumulative
  run and its source value is included as the first value.
- `DECUMULATE(source)` returns `source[t] - source[t-1]`. In the first period it
  returns `source[t]`, because the value before the range is numeric zero.

The optional Anaplan list argument is not in Phase 1.

### 5.4 Period-to-date functions

```text
MONTHTODATE(source)
QUARTERTODATE(source)
YEARTODATE(source)
```

Each function returns an inclusive cumulative sum from the start of the
containing calendar interval through the current period. Determine interval
boundaries from `period_start`, `period_end`, granularity, and
`fiscal_year_start_month`; never parse the member label or code.

- `MONTHTODATE` requires a day-granularity source.
- `QUARTERTODATE` accepts day, week, or month sources.
- `YEARTODATE` accepts day, week, month, quarter, or half-year sources.
- A period that crosses one of the requested boundaries makes the dimension
  invalid for that function; do not prorate it silently.

## 6. Required evaluator design

Add a time-aware callback contract in `internal/formula`, without importing the
calculation package into the formula package. One acceptable shape is:

```go
type TimePeriod struct {
    Code       string
    Index      int
    Start, End time.Time
}

type TimeEvalContext struct {
    DimensionID string
    Position    int
    Periods     []TimePeriod
    EvalAt      func(node Node, position int) Value
}

type EvalContext struct {
    Vars  map[string]Value
    Funcs map[string]CustomFunc
    Time  *TimeEvalContext
}
```

The exact types may differ, but the following behavior is mandatory:

1. Time functions receive unevaluated AST arguments.
2. `EvalAt` evaluates the requested AST node with the time member changed and
   every non-time coordinate unchanged.
3. Identifier lookup at that shifted coordinate resolves input metrics from
   `fact_input`, calculated metrics from the current calculation cache or
   `calc_result`, and the time-dimension variable to the shifted member code.
4. Nested time functions work because the shifted context carries its own
   position.
5. `EvalWithContext` must preserve the time context when it normalizes variable
   keys; its current reconstruction of only `Vars` and `Funcs` would otherwise
   discard it.
6. Add an exported or package-level safe way for a custom function to evaluate
   one AST node in the current context. Do not duplicate the evaluator in the
   calculation package.
7. Memoize `(expression node, time index, non-time coordinate)` inside one
   calculation pass to avoid repeated window evaluation.
8. Add a recursion guard that reports a causal-cycle error instead of
   overflowing the Go stack.

Register Phase 1 names in the built-in function table so
`formula.IsBuiltin` and formula validation agree with evaluation. Unknown or
not-yet-supported time functions must continue to fail validation.

## 7. Required semantic analysis

`ExtractIdents` and `ExtractCalls` are not enough for time formulas. Add a
single AST analysis pass, for example `formula.Analyze`, returning:

```go
type ReferenceUse struct {
    Name            string
    MinTimeOffset   int
    MaxTimeOffset   int
    UnboundedPast   bool
    UnboundedFuture bool
}

type Analysis struct {
    References       []ReferenceUse
    Calls            []string
    UsesTimeSeries   bool
}
```

The analyzer must understand argument roles:

- source expressions inherit the time shift/window of their function;
- substitute and reset expressions stay at offset zero;
- `SUM`, `AVERAGE`, `MIN`, `MAX`, `STRICT`, `SEMISTRICT`, and `NONSTRICT` in
  their defined positions are keywords, not metric references;
- literal offsets are validated and converted to source-relative offsets; and
- nested time functions compose offsets. For example,
  `PREVIOUS(LAG(x, 2, 0))` references `x` at `-3`.

Replace dependency-edge creation in all four save paths with this analyzer:

- developer metric create;
- developer metric update;
- AI Developer `create_metric`; and
- AI Developer `update_metric`.

Do not keep separate partial validators.

## 8. Scheduler and temporal cycles

### 8.1 Ordinary formulas

For an acyclic metric graph, keep dependency-first ordering. Within each
calculated metric, load the time dimension once, group leaf combinations by all
non-time coordinates, and evaluate each group's periods in chronological order.
Use the same `rollup.Resolve` rules for non-time coordinates that the existing
scheduler uses.

Bulk-load each dependency's value map once per metric or recurrence group. A
time lookup should be an in-memory key lookup, not a database query per cell.

### 8.2 Causal recurrences

Anaplan-style opening/closing balances need a cross-metric cycle that is broken
by time:

```text
opening_cash = LAG(closing_cash, 1, 100)
closing_cash = opening_cash + net_cash_flow
```

The current unconditional self-reference and graph-cycle rejection must be
replaced with temporal validation.

For each strongly connected component (SCC) in the metric dependency graph:

1. The zero-offset edges inside the SCC must be acyclic.
2. If every other internal edge points to the past (`max_time_offset < 0`),
   evaluate periods from earliest to latest and use zero-edge topological order
   within each period.
3. If every other internal edge points to the future
   (`min_time_offset > 0`), evaluate periods from latest to earliest.
4. Reject an SCC that mixes past and future edges, has an unbroken zero-offset
   cycle, or contains an unbounded temporal edge.
5. A self-reference is legal only when it has a strictly past or strictly
   future offset under the same rules.

Report `TEMPORAL_CYCLE_NOT_CAUSAL` at formula save or revision validation with
the metric names and offending offsets. Do not defer the problem to the
background scheduler.

Persist all members of a recurrence SCC atomically for one non-time slice, or
stage results in memory and write the complete SCC transactionally. A failed
later period must not leave a half-updated recurrence chain.

### 8.3 Recalculation scope

The current scheduler recomputes the affected metric over all leaf combinations,
which is correct but coarse. Keep that behavior for Phase 1. A future optimizer
may use dependency windows so a change at period `t` recalculates only affected
periods, but correctness comes before partial invalidation.

When a time member is inserted, removed, re-dated, or reindexed, recalculate
every metric dimensioned by that time dimension and all of their dependents.

## 9. Missing values, boundaries, and errors

These rules must be consistent in the grid, chart, export, and API:

- An in-range missing numeric input uses the engine's existing numeric-zero
  contract.
- A position outside the configured time range is out-of-range, not a missing
  fact. `LAG`, `LEAD`, and `OFFSET` use their substitute; `PREVIOUS` and `NEXT`
  use numeric zero; moving windows ignore it.
- An error value in an included period propagates unless the formula handles it
  with `IFERROR`.
- A time function without current time context is a validation error, not zero.
- A source/target time-dimension mismatch is a validation error. Phase 1 does
  not map months to quarters or one calendar to another.
- Empty `MOVINGSUM` ranges return zero. `AVERAGE`, `MIN`, or `MAX` over an empty
  effective range also return zero for Phase 1 numeric compatibility.

Use actionable error identifiers and messages:

```text
TIME_DIMENSION_REQUIRED
MULTIPLE_TIME_DIMENSIONS
TIME_DIMENSION_MISMATCH
INVALID_TIME_MEMBER
DYNAMIC_TIME_OFFSET_UNSUPPORTED
TIME_CONTEXT_REQUIRED
TEMPORAL_CYCLE_NOT_CAUSAL
```

## 10. Security and revision isolation

- Every time-dimension, member, fact, result, and dependency load must be scoped
  to the same model and revision as the target metric.
- A time-series calculation is a model result and is persisted independently
  of the viewing user. Existing member access rules still determine which
  period cells a user may request.
- Before release, test whether a visible derived value can reveal a hidden
  period through `LAG`, `CUMULATE`, or `MOVINGSUM`. If dimension-member hiding
  is a confidentiality boundary, the safe Phase 1 behavior is to suppress a
  result whose required time window includes a hidden period. Do not simply
  omit the hidden source from the arithmetic, because that changes the model.
- Revision cloning, publication, package export/import, and tenant migration
  must preserve the time marker and chronological metadata exactly.

## 11. Exact acceptance example

Create a time dimension with four monthly members and a numeric `sales` input:

| Period | `period_start` | Sales |
|---|---|---:|
| `2026-01` | 2026-01-01 | 100 |
| `2026-02` | 2026-02-01 | 120 |
| `2026-03` | 2026-03-01 | 80 |
| `2026-04` | 2026-04-01 | 150 |

The persisted leaf results must be:

| Formula | Jan | Feb | Mar | Apr |
|---|---:|---:|---:|---:|
| `PREVIOUS(sales)` | 0 | 100 | 120 | 80 |
| `NEXT(sales)` | 120 | 80 | 150 | 0 |
| `LAG(sales, 2, 0)` | 0 | 0 | 100 | 120 |
| `LEAD(sales, 1, 0)` | 120 | 80 | 150 | 0 |
| `OFFSET(sales, -1, 0)` | 0 | 100 | 120 | 80 |
| `MOVINGSUM(sales, -2, 0)` | 100 | 220 | 300 | 350 |
| `MOVINGSUM(sales, -2, 0, AVERAGE)` | 100 | 110 | 100 | 116.6667 |
| `CUMULATE(sales)` | 100 | 220 | 300 | 450 |
| `DECUMULATE(sales)` | 100 | 20 | -40 | 70 |

Run the same fixture with two products. Each product's window must use only
that product's values. This test detects accidental aggregation across
non-time dimensions.

Recurrence fixture:

| Period | Net cash flow | Expected opening | Expected closing |
|---|---:|---:|---:|
| Jan | 10 | 100 | 110 |
| Feb | -20 | 110 | 90 |
| Mar | 5 | 90 | 95 |

with:

```text
opening_cash = LAG(closing_cash, 1, 100)
closing_cash = opening_cash + net_cash_flow
```

## 12. Test plan

### Formula unit tests

- every signature, argument count, keyword, and case variant;
- boundary, negative-offset, strictness, and empty-window behavior;
- nested offsets and composed dependency ranges;
- keyword identifiers excluded from dependency references;
- `EvalWithContext` retaining the time context;
- integer-literal enforcement; and
- error propagation through and around `IFERROR`.

### API and schema tests

- a dimension called `month` but marked `standard` is rejected for `LAG`;
- an arbitrarily named dimension marked `time` works;
- time configuration is required at creation and immutable later;
- invalid, overlapping, duplicate, or gapped regular periods are rejected;
- time members cannot have parents in Phase 1;
- a grid cannot contain two time dimensions;
- OpenAPI, protobuf, manual gateway, generated types, and frontend clients
  serialize the same fields; and
- CSV and package round trips preserve exact dates and order.

### Calculation integration tests

- the exact table in Section 11 at leaf and time-summary levels;
- product/department/entity slices never bleed into each other;
- a changed earlier input updates all affected later cumulative/recurrence
  periods;
- source and result metrics with different time dimensions are rejected;
- past-causal and future-causal SCCs calculate in the correct direction;
- zero, mixed-direction, and unbounded cycles are rejected;
- a time-member edit invalidates and recalculates affected metrics;
- all values are revision-isolated; and
- a failed recurrence cannot persist a partial chain.

### Consumer parity tests

For one seeded model, assert equality across:

- `runtime.calc_result`;
- Planning Grid;
- dashboard chart and KPI;
- CSV/XLSX export;
- workflow approval copy; and
- public/read API.

The browser must not contain a second implementation of time arithmetic.

## 13. Later Anaplan parity — reject until complete

The following are intentionally outside Phase 1:

- dynamic or metric-driven offsets;
- list/dimension arguments that make time functions operate over arbitrary
  standard dimensions;
- `POST`, `SPREAD`, and `PROFILE`, which distribute one source into one or many
  target periods and require reverse accumulation rather than contextual reads;
- `TIMESUM` with absolute time-period literals and targets without a time
  dimension;
- `WEEKVALUE`, `MONTHVALUE`, `QUARTERVALUE`, `HALFYEARVALUE`, and `YEARVALUE`;
- cross-calendar and cross-timescale mapping;
- non-numeric time-series persistence.

Time hierarchies with parent period members, originally listed here, are
implemented as aggregate periods (see the status block).

These names must fail formula validation as unknown/unsupported until their
engine, dependency, summary, security, and consumer-parity tests are present.
Never register a placeholder that returns zero.

## 14. Implementation sequence and file checklist

Implement in this order so each layer has a testable contract:

1. **Schema and domain invariants**
   - add migration 087 (or the next available number);
   - update migration test fixtures;
   - add shared dimension/time validation.
2. **API, transfer, and UI metadata**
   - update `api/openapi.yaml`, `proto/model/v1/model.proto`, manual handlers,
     generated clients, AI tool schemas, `DimensionsTab.tsx`, and client types;
   - update `internal/modeltransfer/transfer.go` and revision-copy SQL.
3. **Formula analysis and evaluator context**
   - add `internal/formula/time.go`, `time_test.go`, and semantic analysis;
   - preserve scalar behavior for callers that provide no time context.
4. **Validation and dependency graph**
   - make `internal/metricformula` the single save-time validator;
   - persist offset ranges and implement temporal SCC validation.
5. **Calculation execution**
   - extend `internal/calculation/store.go`, `evaluator.go`, and `scheduler.go`;
   - add time-slice caching, recurrence execution, and time summaries.
6. **Read-path convergence**
   - serve persisted results to grids, charts, exports, workflows, and APIs;
   - do not add client-side time formulas.
7. **Demo and documentation**
   - replace the Sales Planning demo's manual `prior_revenue` workaround with
     `PREVIOUS(revenue)` or `LAG(revenue, 1, 0)`;
   - update `cmd/seed-sales-planning`, `internal/salesdemo`, formula help, and
     the generated Developer Manual only after tests pass.

## 15. Definition of done

Phase 1 is complete only when:

- the developer explicitly creates a Time dimension in the console or API;
- the time metadata survives revision copy and package round trips;
- all Phase 1 functions validate and return the specified persisted values;
- causal opening/closing balance recurrence works;
- unsupported time syntax is rejected before calculation;
- consumer parity and hidden-member security tests pass;
- the Sales Planning workaround is removed; and
- current documentation changes from “not implemented” to “implemented” in
  the same commit that enables the feature.

## 16. Compatibility references

The signatures and baseline behavior above are informed by official Anaplan
documentation for [LAG](https://help.anaplan.com/lag-3064919f-964e-4b84-be56-15f0e127e371),
[LEAD](https://help.anaplan.com/lead-e3f4969b-65b1-4726-b41c-d028c9c71c14),
[OFFSET](https://help.anaplan.com/offset-4f5a095c-0e7a-4f1a-b6ea-0ef8f88d6c3f),
[MOVINGSUM](https://help.anaplan.com/movingsum-37394929-ea62-4e55-9655-b8c8c2732679),
[CUMULATE](https://help.anaplan.com/cumulate-1173a903-81bb-4838-a4d0-1c9f9c739aa3),
[PREVIOUS](https://help.anaplan.com/previous-e5806da3-1ae6-4b45-9e02-68ac764cb97d),
[NEXT](https://help.anaplan.com/next-ce38460d-e931-403a-837c-d650d0ddaf64),
and [DECUMULATE](https://help.anaplan.com/decumulate-eab1f7ce-5c1d-46b6-8361-69086d4876e7).
Where Anaplan Classic and Polaris differ, this document states the Mavericks
choice explicitly rather than inheriting an ambiguous default.
