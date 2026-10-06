// Package startersync keeps the starter models (internal/starter: the tour
// and one guide per role) of every tenant that signed up for itself present
// and current, whenever it signed up.
//
// Sign-up imports the starters of its day. On each start of the gateway,
// Run looks at every such tenant of a database and, per starter:
//
//   - one it never had is imported into its "Getting started" application
//     (made again if it was deleted) — once: core.starter_model remembers
//     it, so a model the tenant deletes later is not put back;
//   - one whose content has changed since it was installed gets the current
//     content as a new revision of the same model, made live. The old
//     revision stays, with anything typed into it. A model where someone has
//     since made another revision live is the tenant's own, and is left
//     alone;
//   - a starter model a tenant got before this record existed is found by
//     its name in the "Getting started" application and brought up to date
//     the same way.
//
// Each tenant is one transaction under an advisory lock, so replicas
// starting together do the work once.
package startersync

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
	"github.com/mavericks-engine/mavericks/internal/modeltransfer"
	"github.com/mavericks-engine/mavericks/internal/starter"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// AppName is the application sign-up puts the starters in.
const AppName = "Getting started"

// Hash names a starter's content: two packages with the same hash import
// the same model. A package is plain data built the same way every time
// (json.Marshal sorts map keys), so the hash changes only with the content.
func Hash(pkg modeltransfer.Package) string {
	b, _ := json.Marshal(pkg)
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum)
}

// Record notes in tx that the tenant holds starter key as modelID, brought
// to the content hash at revisionID. Sign-up records what it imports.
func Record(ctx context.Context, tx pgx.Tx, customerID, key, modelID, revisionID, hash string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO core.starter_model (customer_id, starter_key, model_id, revision_id, content_hash)
		VALUES ($1::uuid, $2, NULLIF($3, '')::uuid, NULLIF($4, '')::uuid, $5)
		ON CONFLICT (customer_id, starter_key) DO UPDATE
		SET model_id = EXCLUDED.model_id, revision_id = EXCLUDED.revision_id,
		    content_hash = EXCLUDED.content_hash, updated_at = now()`,
		customerID, key, modelID, revisionID, hash)
	return err
}

// Change is one model Sync installed or brought up to date: a revision whose
// calculated metrics need computing.
type Change struct {
	CustomerID, StarterKey, ModelID, RevisionID string
	Installed                                   bool // false: a new revision of a model the tenant had
}

// Run syncs every self-service tenant of one database, then recalculates
// what changed. Failures are logged per tenant and never stop the start-up.
func Run(ctx context.Context, pool *pgxpool.Pool, log zerolog.Logger) {
	customers, err := selfServiceTenants(ctx, pool)
	if err != nil {
		log.Warn().Err(err).Msg("starter sync: list tenants")
		return
	}
	starters := starter.Starters()
	var changes []Change
	for _, c := range customers {
		if ctx.Err() != nil {
			return
		}
		got, err := Sync(ctx, pool, c, starters, time.Now())
		if err != nil {
			log.Warn().Err(err).Str("tenant", c).Msg("starter sync: tenant not brought up to date")
			continue
		}
		changes = append(changes, got...)
	}
	if len(changes) == 0 {
		return
	}
	Recalculate(ctx, pool, log, changes)
	log.Info().Int("models", len(changes)).Msg("starter sync: starter models installed or brought up to date")
}

// Recalculate computes every calculated metric of each changed revision, as
// sign-up does for the starters it imports.
func Recalculate(ctx context.Context, pool *pgxpool.Pool, log zerolog.Logger, changes []Change) {
	sched := calculation.NewScheduler(log, calculation.NewStore(pool), nil)
	for _, ch := range changes {
		rows, err := pool.Query(ctx,
			`SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND NOT is_input`,
			ch.ModelID, ch.RevisionID)
		if err != nil {
			log.Warn().Err(err).Str("revision", ch.RevisionID).Msg("starter sync: load metrics")
			continue
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(ids) == 0 {
			continue
		}
		if err := sched.RecalcSpecific(ctx, ch.ModelID, ch.RevisionID, ids); err != nil {
			log.Warn().Err(err).Str("revision", ch.RevisionID).Msg("starter sync: recalc")
		}
	}
}

// selfServiceTenants are the tenants of this database that signed up for
// themselves: sign-up's audit event names each, and its starter records do.
func selfServiceTenants(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT c.id::text FROM core.customer c
		WHERE EXISTS (SELECT 1 FROM audit.audit_event e
		              WHERE e.event_type = $1 AND e.resource_type = 'tenant' AND e.resource_id = c.id::text)
		   OR EXISTS (SELECT 1 FROM core.starter_model s WHERE s.customer_id = c.id)
		ORDER BY c.created_at`, string(auditlog.EventTenantSignedUp))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

type record struct {
	modelID, revisionID *string
	hash                string
}

// Sync brings one tenant's starters up to date in one transaction and
// returns what it changed. now dates the name of a new revision.
func Sync(ctx context.Context, pool *pgxpool.Pool, customerID string, starters []starter.Starter, now time.Time) ([]Change, error) {
	var changes []Change
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		changes = nil
		// One replica per tenant at a time; the second reads what the first wrote.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('startersync'), hashtext($1))`, customerID); err != nil {
			return fmt.Errorf("lock: %w", err)
		}
		records := map[string]record{}
		rows, err := tx.Query(ctx, `
			SELECT starter_key, model_id::text, revision_id::text, content_hash
			FROM core.starter_model WHERE customer_id = $1::uuid`, customerID)
		if err != nil {
			return fmt.Errorf("read starter records: %w", err)
		}
		for rows.Next() {
			var key string
			var r record
			if err := rows.Scan(&key, &r.modelID, &r.revisionID, &r.hash); err != nil {
				rows.Close()
				return err
			}
			records[key] = r
		}
		rows.Close()

		t := &tenant{tx: tx, customerID: customerID}
		for _, s := range starters {
			hash := Hash(s.Package)
			rec, known := records[s.Key]
			if !known {
				// Installed before records were kept: find it by name.
				modelID, revisionID, err := t.findByName(ctx, s.Package.ModelName)
				if err != nil {
					return err
				}
				if modelID != "" {
					rec = record{modelID: &modelID, revisionID: &revisionID}
					if err := Record(ctx, tx, customerID, s.Key, modelID, revisionID, ""); err != nil {
						return fmt.Errorf("record %s: %w", s.Key, err)
					}
					known = true
				}
			}
			switch {
			case !known:
				ch, err := t.install(ctx, s, hash)
				if err != nil {
					return fmt.Errorf("install %s: %w", s.Key, err)
				}
				changes = append(changes, ch)
			case rec.modelID == nil:
				// The tenant deleted it: not put back.
			case rec.hash == hash:
				// Current.
			default:
				ch, ok, err := t.update(ctx, s, hash, *rec.modelID, rec.revisionID, now)
				if err != nil {
					return fmt.Errorf("update %s: %w", s.Key, err)
				}
				if ok {
					changes = append(changes, ch)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return changes, nil
}

type tenant struct {
	tx         pgx.Tx
	customerID string
	importer   string // resolved on first use
	appID      string // resolved on first use
}

// findByName is the tenant's model of that name in a "Getting started"
// application that no starter record names yet.
func (t *tenant) findByName(ctx context.Context, modelName string) (modelID, revisionID string, err error) {
	err = t.tx.QueryRow(ctx, `
		SELECT m.id::text, COALESCE(m.active_revision_id::text, '')
		FROM core.model m JOIN core.application a ON a.id = m.application_id
		WHERE a.customer_id = $1::uuid AND a.name = $2 AND m.name = $3
		  AND NOT EXISTS (SELECT 1 FROM core.starter_model s WHERE s.model_id = m.id)
		ORDER BY m.created_at LIMIT 1`, t.customerID, AppName, modelName).Scan(&modelID, &revisionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	return modelID, revisionID, err
}

// importerID is who the imported rows name as having entered them: the
// person who signed the tenant up, else one of its tenant admins.
func (t *tenant) importerID(ctx context.Context) (string, error) {
	if t.importer != "" {
		return t.importer, nil
	}
	err := t.tx.QueryRow(ctx, `
		SELECT u.id::text FROM identity.user u
		WHERE u.customer_id = $1::uuid AND (
		      u.id IN (SELECT e.actor_user_id FROM audit.audit_event e
		               WHERE e.event_type = $2 AND e.resource_type = 'tenant' AND e.resource_id = $1::text)
		   OR u.id IN (SELECT ra.user_id FROM identity.role_assignment ra WHERE ra.role = 'tenant_admin'))
		ORDER BY (u.id IN (SELECT e.actor_user_id FROM audit.audit_event e
		                   WHERE e.event_type = $2 AND e.resource_type = 'tenant' AND e.resource_id = $1::text)) DESC,
		         u.created_at
		LIMIT 1`, t.customerID, string(auditlog.EventTenantSignedUp)).Scan(&t.importer)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("the tenant has no one to import as: neither who signed it up nor a tenant admin")
	}
	return t.importer, err
}

// application is the tenant's "Getting started" application, made again in
// its first workspace when it has none.
func (t *tenant) application(ctx context.Context) (string, error) {
	if t.appID != "" {
		return t.appID, nil
	}
	err := t.tx.QueryRow(ctx, `
		SELECT id::text FROM core.application WHERE customer_id = $1::uuid AND name = $2
		ORDER BY created_at LIMIT 1`, t.customerID, AppName).Scan(&t.appID)
	if err == nil {
		return t.appID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	err = t.tx.QueryRow(ctx, `
		INSERT INTO core.application (customer_id, workspace_id, name, mode)
		SELECT $1::uuid, w.id, $2, 'planning'::core.application_mode
		FROM core.workspace w WHERE w.customer_id = $1::uuid
		ORDER BY w.created_at LIMIT 1
		RETURNING id::text`, t.customerID, AppName).Scan(&t.appID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("the tenant has no workspace to put %q in", AppName)
	}
	return t.appID, err
}

// install imports a starter the tenant never had. The tour becomes the
// application's business default when nothing else is.
func (t *tenant) install(ctx context.Context, s starter.Starter, hash string) (Change, error) {
	importer, err := t.importerID(ctx)
	if err != nil {
		return Change{}, err
	}
	appID, err := t.application(ctx)
	if err != nil {
		return Change{}, err
	}
	modelID, revisionID, err := modeltransfer.Import(ctx, t.tx, modeltransfer.ImportRequest{ApplicationID: appID, Package: s.Package}, importer)
	if err != nil {
		return Change{}, err
	}
	if s.Key == starter.LandingKey {
		if _, err := t.tx.Exec(ctx, `
			UPDATE core.application SET default_model_id = $2::uuid
			WHERE id = $1::uuid AND default_model_id IS NULL`, appID, modelID); err != nil {
			return Change{}, fmt.Errorf("set default model: %w", err)
		}
	}
	if err := Record(ctx, t.tx, t.customerID, s.Key, modelID, revisionID, hash); err != nil {
		return Change{}, err
	}
	auditlog.Log(ctx, t.tx, zerolog.Nop(), auditlog.Fields{
		Category: auditlog.CategoryModelChange, EventType: auditlog.EventModelImported,
		ActorRole: "platform(starter sync)", ApplicationID: appID,
		ResourceType: "model", ResourceID: modelID, RevisionID: revisionID,
		Metadata: map[string]string{"source": "starter", "starter": s.Key, "model_name": s.Package.ModelName},
	})
	return Change{CustomerID: t.customerID, StarterKey: s.Key, ModelID: modelID, RevisionID: revisionID, Installed: true}, nil
}

// update adds the starter's current content as a new revision of the model
// that holds it and makes it live, as Set active does: time check, the
// pointer, access rules moved by lineage, and the business roles' dashboard
// grants carried by dashboard name, as a revision copy carries them. ok is
// false when the model's live revision is no longer the one the sync put
// there: someone made their own live, and the model is theirs.
func (t *tenant) update(ctx context.Context, s starter.Starter, hash, modelID string, installed *string, now time.Time) (Change, bool, error) {
	var live *string
	if err := t.tx.QueryRow(ctx, `SELECT active_revision_id::text FROM core.model WHERE id = $1::uuid`, modelID).Scan(&live); err != nil {
		return Change{}, false, fmt.Errorf("read model: %w", err)
	}
	if live == nil || installed == nil || *live != *installed {
		return Change{}, false, nil
	}
	importer, err := t.importerID(ctx)
	if err != nil {
		return Change{}, false, err
	}
	pkg, err := withLineages(ctx, t.tx, s.Package, modelID, *live)
	if err != nil {
		return Change{}, false, err
	}
	name, err := revisionName(ctx, t.tx, modelID, now)
	if err != nil {
		return Change{}, false, err
	}
	revisionID, err := modeltransfer.ImportRevision(ctx, t.tx, modelID, name, pkg, importer)
	if err != nil {
		return Change{}, false, err
	}
	if err := metricformula.ValidateTime(ctx, t.tx, modelID, revisionID); err != nil {
		return Change{}, false, fmt.Errorf("new revision: %w", err)
	}
	if _, err := t.tx.Exec(ctx, `
		UPDATE core.model SET active_revision_id = $2::uuid, active_revision_name = $3 WHERE id = $1::uuid`,
		modelID, revisionID, name); err != nil {
		return Change{}, false, fmt.Errorf("make live: %w", err)
	}
	if err := writeguard.RemapRulesToRevision(ctx, t.tx, modelID, revisionID); err != nil {
		return Change{}, false, fmt.Errorf("remap access rules: %w", err)
	}
	if _, err := t.tx.Exec(ctx, `
		INSERT INTO identity.business_role_dashboard (role_id, dashboard_id)
		SELECT brd.role_id, nd.id
		FROM identity.business_role_dashboard brd
		JOIN model.dashboard_def od ON od.id = brd.dashboard_id
		JOIN model.dashboard_def nd ON nd.model_id = od.model_id AND nd.name = od.name AND nd.revision_id = $2::uuid
		WHERE od.model_id = $1::uuid AND od.revision_id = $3::uuid
		ON CONFLICT DO NOTHING`, modelID, revisionID, *live); err != nil {
		return Change{}, false, fmt.Errorf("carry dashboard grants: %w", err)
	}
	if err := Record(ctx, t.tx, t.customerID, s.Key, modelID, revisionID, hash); err != nil {
		return Change{}, false, err
	}
	var appID string
	_ = t.tx.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id = $1::uuid`, modelID).Scan(&appID)
	auditlog.Log(ctx, t.tx, zerolog.Nop(), auditlog.Fields{
		Category: auditlog.CategoryModelChange, EventType: auditlog.EventRevisionActivated,
		ActorRole: "platform(starter sync)", ApplicationID: appID,
		ResourceType: "revision", ResourceID: revisionID, RevisionID: revisionID,
		Metadata: map[string]string{"source": "starter", "starter": s.Key, "previous_revision_id": *live, "revision_name": name},
	})
	return Change{CustomerID: t.customerID, StarterKey: s.Key, ModelID: modelID, RevisionID: revisionID}, true, nil
}

// withLineages is a copy of pkg whose dimensions, members and metrics carry
// the lineage of their namesakes in the model's live revision — dimensions
// by name, members by dimension and code, metrics by name in any case — so
// the new revision lines up with it as a copy of it would.
func withLineages(ctx context.Context, tx pgx.Tx, pkg modeltransfer.Package, modelID, revisionID string) (modeltransfer.Package, error) {
	raw, err := json.Marshal(pkg)
	if err != nil {
		return pkg, err
	}
	var out modeltransfer.Package
	if err := json.Unmarshal(raw, &out); err != nil {
		return pkg, err
	}
	lineages := func(sql string) (map[string]string, error) {
		rows, err := tx.Query(ctx, sql, modelID, revisionID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		m := map[string]string{}
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				return nil, err
			}
			m[k] = v
		}
		return m, rows.Err()
	}
	dims, err := lineages(`SELECT name, lineage_id::text FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND lineage_id IS NOT NULL`)
	if err != nil {
		return pkg, fmt.Errorf("dimension lineages: %w", err)
	}
	members, err := lineages(`
		SELECT d.name || E'\x1f' || m.code, m.lineage_id::text
		FROM model.dimension_member m JOIN model.dimension_def d ON d.id = m.dimension_id
		WHERE d.model_id=$1::uuid AND d.revision_id=$2::uuid AND m.lineage_id IS NOT NULL`)
	if err != nil {
		return pkg, fmt.Errorf("member lineages: %w", err)
	}
	metrics, err := lineages(`SELECT lower(name), lineage_id::text FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND lineage_id IS NOT NULL`)
	if err != nil {
		return pkg, fmt.Errorf("metric lineages: %w", err)
	}
	for i := range out.Dimensions {
		d := &out.Dimensions[i]
		if l, ok := dims[d.Name]; ok {
			d.LineageID = l
		}
		for j := range d.Members {
			if l, ok := members[d.Name+"\x1f"+d.Members[j].Code]; ok {
				d.Members[j].LineageID = l
			}
		}
	}
	for i := range out.Metrics {
		if l, ok := metrics[strings.ToLower(out.Metrics[i].Name)]; ok {
			out.Metrics[i].LineageID = l
		}
	}
	return out, nil
}

// revisionName is "Updated <date>", made unique within the model.
func revisionName(ctx context.Context, tx pgx.Tx, modelID string, now time.Time) (string, error) {
	base := "Updated " + now.UTC().Format("2006-01-02")
	for n := 1; n < 100; n++ {
		name := base
		if n > 1 {
			name = fmt.Sprintf("%s (%d)", base, n)
		}
		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model.revision WHERE model_id=$1::uuid AND name=$2)`, modelID, name).Scan(&taken); err != nil {
			return "", err
		}
		if !taken {
			return name, nil
		}
	}
	return "", fmt.Errorf("no free revision name for %s", base)
}
