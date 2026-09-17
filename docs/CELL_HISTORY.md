# Per-cell change history

> **Classification:** Current — Who changed which value, browsable per intersection.

> **Last verified:** 2026-09-16

An enterprise feature (`cell_history`). Right-click an input cell in a
planning grid and choose **History** to see every value that intersection
has held: when, who, through what, and — for values that were later removed
— why.

## Where the history comes from

Nothing new is captured. `runtime.fact_input` has always been append-only:
every write is a new row, and every reader takes the latest row
(`entered_at DESC, id DESC`). The history of a cell is therefore all of its
rows in that order, with the first one being what the grid shows.

The one hole was deletion. Three paths delete rows — an import in
**full-reload** mode, a form integration **re-posting** its aggregates, and
**re-parenting** a dimension member (its facts are re-keyed) — and a deleted
row was a change that vanished from the record. Migration 081 adds
`runtime.fact_input_history` and a `BEFORE DELETE` trigger that copies every
deleted row there, with the reason the deleting transaction declared
(`SET LOCAL mvx.delete_reason`). The archive has no foreign keys on purpose:
deleting a metric or a model must not fail because its history exists.

## What a row says

| Field | Meaning |
|---|---|
| `entered_by` | the account that wrote the row |
| `source.kind` | `typed` — a person in the grid, or an import they ran; `form` — a form integration's posting, with the mapping's name; `copied` — the row came with a revision copy (it predates the revision) |
| `current` | the row the grid shows now |
| `deleted_at`, `delete_reason` | for archived rows: when and why |

Imports are attributed to the person who ran them and show as `typed`;
distinguishing them would need a source marker on the import path, which is
not there yet.

## Access

`GET /api/cells/history` applies the grid's own rules: the caller must have
access to the model, the revision must belong to it, the metric must be an
input, and every member of the intersection must be visible to the caller
(a hidden member answers 403 exactly as the grid would not show it). Reads
are not audited.

## Where the code lives

- `ee/cellhistory` — the query (live rows ∪ archive) and source labelling.
- `internal/gateway/cell_history.go` — the route, behind
  `requireFeature(license.FeatureCellHistory)`.
- `migrations/081_cell_history.sql` — the archive and the trigger; the three
  delete sites declare their reason.
- `web/src/ee/cellhistory/CellHistoryDrawer.tsx` — the drawer;
  `PlanningGrid.tsx` opens it from a cell's context menu.
