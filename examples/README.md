# Example models

> **Classification:** Current — Model packages you can import to try the platform, and how to share your own.

A model package is a JSON file holding a model's definitions: dimensions and
their members, metrics and formulas, grids, dashboards, forms, workflows and
automation rules. Import one and you have a working model in seconds, without
building it by hand.

| File | What it is |
| --- | --- |
| [`sales-planning.mavericks-model.json`](sales-planning.mavericks-model.json) | Sales planning: Geography, Product and a Period time dimension (FY → H → Q), 34 metrics using most of the formula functions (rollups that are not sums, time offsets, forecasts, scenarios), and one planning grid. Definitions only, no values. |

## Import a model

1. Sign in as a tenant administrator and open **Administration › Applications**.
2. Choose the application the model should go into and select **Import model**.
3. Pick the `.mavericks-model.json` file.

The import creates a new model with its own first revision. Nothing in the
file can reach existing models: every ID in it is replaced on import.

## Share your own model

1. In **Administration › Applications**, open the model and find the revision
   you want to share.
2. Export it with **definitions only**. A full export also carries the values
   people entered and the records of every form; a definitions-only export
   carries neither.
3. Open the file and check what remains: names of dimension members, metrics
   and dashboards, and the settings of any integrations (endpoint addresses,
   never credentials, which stay in the installation that holds them).
4. Post it in [**Show and tell**](https://github.com/Mavericka-SARL/maverick-builds-app/discussions/categories/show-and-tell)
   with a sentence on what the model is for and the release you exported it from.

## How the example here is made

`sales-planning.mavericks-model.json` is the demo in
[`internal/salesdemo`](../internal/salesdemo), built through the HTTP API and
exported definitions-only, the way a tenant administrator does it. Its IDs are
replaced by fixed ones so that each export gives the same file. The test
`TestSalesDemoExamplePackage` (in `internal/gateway`) checks it against a fresh
export and imports it; after a change to the demo or to the package format,
regenerate it with:

```sh
UPDATE_EXAMPLES=1 go test ./internal/gateway -run TestSalesDemoExamplePackage
```
