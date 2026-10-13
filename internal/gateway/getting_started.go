package gateway

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/modeltransfer"
	"github.com/mavericks-engine/mavericks/internal/starter"
	"github.com/mavericks-engine/mavericks/internal/startersync"
)

// The "Getting started" application: the starter models (starter.Starters —
// the "Learn the platform" tour and one guide per role), with the tour,
// starter.LandingKey, as the application's default and what a newcomer
// first sees. Sign-up installs it in every tenant it makes, and a platform
// administrator's new tenant gets it unless they say otherwise
// (adminTenants), so a self-hosted deployment starts where a Playground
// sign-up does. internal/startersync keeps it up to date from then on, by
// what installGettingStarted records.

// gettingStarted is what installGettingStarted made: the application and the
// starters' models, in starter.Starters order; landing indexes the one the
// application opens on.
type gettingStarted struct {
	appID   string
	models  []starterModel
	landing int
}

type starterModel struct{ key, modelID, revisionID string }

// installGettingStarted makes, in tx on the tenant's database, the "Getting
// started" application in workspaceID, imports every starter into it as
// importerID (whose rows the import names as their author) and makes the
// tour its default. All of it or none: tx is the caller's.
func installGettingStarted(ctx context.Context, tx pgx.Tx, customerID, workspaceID, importerID string) (gettingStarted, error) {
	gs := gettingStarted{landing: -1}
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.application (customer_id, workspace_id, name, mode)
		VALUES ($1::uuid, $2::uuid, $3, 'planning'::core.application_mode) RETURNING id::text`,
		customerID, workspaceID, startersync.AppName).Scan(&gs.appID); err != nil {
		return gs, fmt.Errorf("create application: %w", err)
	}
	for _, s := range starter.Starters() {
		mID, rID, err := modeltransfer.Import(ctx, tx, modeltransfer.ImportRequest{ApplicationID: gs.appID, Package: s.Package}, importerID)
		if err != nil {
			return gs, fmt.Errorf("starter model %q: %w", s.Package.ModelName, err)
		}
		// Recorded, so later content reaches this tenant as a new revision
		// (internal/startersync) rather than not at all.
		if err := startersync.Record(ctx, tx, customerID, s.Key, mID, rID, startersync.Hash(s.Package)); err != nil {
			return gs, fmt.Errorf("record starter model %q: %w", s.Package.ModelName, err)
		}
		if s.Key == starter.LandingKey {
			gs.landing = len(gs.models)
		}
		gs.models = append(gs.models, starterModel{s.Key, mID, rID})
	}
	if gs.landing < 0 {
		return gs, fmt.Errorf("no starter %q to land on", starter.LandingKey)
	}
	// They share one created_at (the transaction's), so "newest model"
	// cannot pick the one to land on: name it as the application's default,
	// the same setting a developer changes later under Developer › Models.
	if _, err := setApplicationDefaultModel(ctx, tx, gs.models[gs.landing].modelID); err != nil {
		return gs, fmt.Errorf("set default model: %w", err)
	}
	return gs, nil
}

// landingModelID is the model the application opens on.
func (gs gettingStarted) landingModelID() string { return gs.models[gs.landing].modelID }

// guideModelIDs are the other starters' models, in starter.Starters order.
func (gs gettingStarted) guideModelIDs() []string {
	out := make([]string, 0, len(gs.models)-1)
	for i, m := range gs.models {
		if i != gs.landing {
			out = append(out, m.modelID)
		}
	}
	return out
}

// recalcGettingStarted computes every calculated metric of every starter,
// once installGettingStarted's transaction has committed — not only those an
// input reaches: recalcRevisionFromInputs skips a model with no input
// metric, and misses the dependents of a metric that reads only dimensions
// or properties. recalcRevisionCalculated recomputes the whole set in
// dependency order and logs its own failures, which leave the tenant
// standing.
func (h *handler) recalcGettingStarted(ctx context.Context, gs gettingStarted) {
	for _, m := range gs.models {
		h.recalcRevisionCalculated(ctx, m.modelID, m.revisionID)
	}
}

// everyTenantRole is what one person needs to build and run an application
// on their own: a sign-up holds all of them, and so does a platform
// administrator who takes the tenant they create as their own.
// business_user is the user role: the console's User group comes from it
// alone, not from business_admin.
var everyTenantRole = []string{"tenant_admin", "developer", "business_admin", "business_user"}

// grantEveryTenantRole gives userID every tenant role, in tx: the business
// roles in workspaceID, the others tenant-wide.
func grantEveryTenantRole(ctx context.Context, tx pgx.Tx, userID, workspaceID, assignedBy string) error {
	for _, role := range everyTenantRole {
		ws := ""
		if role == "business_admin" || role == "business_user" {
			ws = workspaceID
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO identity.role_assignment (user_id, role, workspace_id, assigned_by)
			VALUES ($1::uuid, $2::identity.user_role, NULLIF($3,'')::uuid, $4::uuid) ON CONFLICT DO NOTHING`,
			userID, role, ws, assignedBy); err != nil {
			return fmt.Errorf("assign %s: %w", role, err)
		}
	}
	return nil
}
