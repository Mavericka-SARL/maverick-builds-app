# Mavericks UI Design System

> **Status:** Current frontend UI authority
> **Last verified:** 2026-07-15
>
> All role consoles use the shared shell, but this is still a migration target,
> not a claim that every screen exposes every context item or contains no local
> styling. `AppPicker` is exported but currently unused; application selection
> remains console-specific. Configuration-save exceptions are tracked in the
> root `IMPLEMENTATION_PLAN.md`.

This package is the implementation target from `UX_UPGRADE_INSTRUCTIONS.md`.
It gives the app one shared planning-product language for shell layout, context,
controls, data states, tables, builder panels, and status badges.

## Principles

- Use one shell for all roles.
- Keep planning context visible: application, model, revision, version, role,
  and live/draft status.
- Use dense tables, tree tables, toolbars, and property panels for operational
  work.
- Use cards only for bounded panels, repeated summary objects, and dashboard
  widgets.
- Keep runtime filters immediate. Keep configuration edits staged with Save and
  Cancel.
- Use Lucide icons for common actions.
- Prefer shared primitives over raw `button`, `input`, `select`, or `textarea`.

## Files

- `design-system.css`: tokens and shared classes.
- `Button.tsx` and `IconButton.tsx`: primary, secondary, ghost, and destructive
  commands with standard sizes.
- `Badge.tsx` and `StatusBadge.tsx`: general, status, role, and revision badges.
- `Dialog.tsx`, `ConfirmDialog.tsx`, and `useConfirm.ts`: modal and destructive
  confirmation flows.
- `Drawer.tsx`: right-side details/edit surface.
- `Toast.tsx`, `ToastContext.tsx`, and `useToast.ts`: application feedback.
- `Field.tsx`, `TextInput.tsx`, `Select.tsx`, and `FormControls.tsx`: shared form
  controls.
- `Card.tsx`, `Tabs.tsx`, and `Table.tsx`: compact base containers/navigation.
- `EmptyState.tsx`, `LoadingState.tsx`, and `ErrorState.tsx`: standard data
  states.
- `UnsavedChangesBar.tsx`: staged-edit save/discard contract.
- `AppPicker.tsx`: shared application selector contract; exported but not wired
  into the current consoles.
- `AppShell.tsx`: global role-agnostic product shell.
- `SidebarNav.tsx`: grouped navigation.
- `ContextBar.tsx`: application/model/revision/status context.
- `PageLayout.tsx`: page and section headers.
- `Toolbar.tsx`: toolbars, filter bars, filter chips.
- `SearchInput.tsx`: search field with icon.
- `SegmentedControl.tsx`: design/preview and mode controls.
- `StatusBadge.tsx`: status, role, and revision badges.
- `DataTable.tsx`: dense tabular data.
- `TreeTable.tsx`: hierarchy tables.
- `FormControls.tsx`: number input, textarea, checkbox, switch.
- `PropertyPanel.tsx`: right-side builder/admin edit panel
  (`mvx-property-panel--floating` variant for panels inside a flex row).
- `SplitPane.tsx`: main plus side-panel layout.
- `InlineAlert.tsx`: info/success/warning/danger callouts.
- `Skeleton.tsx`: loading placeholder.
- `Stepper.tsx`: wizard step indicator (Import Wizard).
- `CommandButton.tsx`: fill-parent action button for dashboard command widgets.

Shared class groups in `design-system.css` beyond component styles:

- `mvx-admin-*`: tenant/application/model/revision object cards and inline forms.
- `mvx-wf-*`: workflow instance steps and pending-action areas.
- `mvx-cell-*` / `mvx-grid-status-*`: planning grid cell states
  (editable/active, readonly, aggregate, calc, empty) and writeback status.
- `mvx-pivot-*`: pivot panel drop zones and draggable chips.
- `mvx-widget__*` / `mvx-kpi__*`: dashboard widget chrome and KPI layout.
- `mvx-chat-*`: AI assistant message bubbles.
- `mvx-stepper*` / `mvx-dropzone*`: wizard step indicator and file drop zone.
- `mvx-source-tile*`: integration source picker tiles.
- `mvx-context-banner`: brand/warning banner naming the active app/revision.
- `mvx-prop-section`: uppercase section label inside property panels.
- `mvx-tree-toggle`: expand/collapse chevron in tree tables.

## Regression gates

`web/e2e/ux-gates.spec.ts` checks role shells at 1440/1280/768 viewports, no
horizontal overflow in selected states, visible focus, an accessible confirm
flow, shared grid cell state classes, import stepper/dropzone, selected AI
states, and per-file hard-coded-color budgets. These mocked dev-mode checks do
not cover every console tab, real Keycloak, or full database workflows.

`chartTypes.ts` has an explicit data-visualization palette exception. Existing
console files also have temporary numerical color budgets so the gate prevents
regression while legacy styles are migrated; a passing budget does not make
those colors design-system tokens. Lower budgets as files are cleaned and do
not raise them without review.

## Example Shell

```tsx
<AppShell
  productSubtitle="Developer console"
  navGroups={[
    { label: "Plan", items: [{ id: "dashboards", label: "Dashboards" }] },
    { label: "Build", items: [{ id: "metrics", label: "Metrics" }] },
  ]}
  activeNavId="dashboards"
  contextItems={[
    { id: "app", label: "Application", value: "Planning" },
    { id: "revision", label: "Revision", value: "FY2026 Budget", tone: "draft" },
  ]}
>
  <PageLayout title="Dashboards" actions={<Button variant="primary">Create</Button>}>
    ...
  </PageLayout>
</AppShell>
```

## Migration Rule

When a console is touched, migrate repeated local UI first:

1. Sidebar to `AppShell` and `SidebarNav`.
2. Page title/actions to `PageLayout`.
3. Filters/search/actions to `Toolbar`, `SearchInput`, and `FilterChip`.
4. Lists to `DataTable` or `TreeTable`.
5. Edit/details areas to `Drawer`, `Dialog`, or `PropertyPanel`.
6. Status text and role labels to `StatusBadge`, `RoleBadge`, or
   `RevisionBadge`.

Avoid adding new hard-coded colors or one-off button/input styles in console
files.
