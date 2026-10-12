// Package dormancy retires workspaces nobody uses.
//
// A plan may set dormant_warn_days and dormant_delete_days (platform.plan;
// both 0, every plan's default, means never). After dormant_warn_days without
// use — a request from the tenant's own people or API clients, which
// tenantdb.Router.Used records; not its scheduled work, and not a platform
// administrator looking at it — the tenant's administrators are told by mail
// that it will be deleted. Once dormant_delete_days have passed without use,
// and at least the notice period since the mail, it is deleted through the
// one tenant deletion path, and they are told again. Any use in between
// withdraws the notice.
//
// Only tenants with a database of their own are covered: their use is what
// platform.tenant_database records. Without a mail relay nothing is retired —
// a workspace is never deleted without notice.
package dormancy

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// Mailer sends one plain-text mail (notification.Mailer).
type Mailer interface {
	Send(ctx context.Context, to, displayName, subject, body string) error
}

// Deps is what a sweep needs.
type Deps struct {
	Control *pgxpool.Pool
	Router  *tenantdb.Router
	Mailer  Mailer
	// Delete removes a tenant through the one deletion path.
	Delete func(ctx context.Context, customerID string) error
	// ConsoleURL is where the tenant's people sign in to keep it.
	ConsoleURL string
	// ContactEmail is where to ask for a restore; empty leaves it unsaid.
	ContactEmail string
	// BackupRetentionDays is how long backups keep a deleted workspace.
	BackupRetentionDays int
	Log                 zerolog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// sweepLock keeps two gateway replicas from sweeping at once, so a tenant is
// told once and deleted once.
const sweepLock = 731_402_118

// Result counts what one sweep did.
type Result struct {
	Warned, Deleted int
}

type candidate struct {
	id, name, plan       string // plan is the plan's display name
	lastUsed             time.Time
	warnDays, deleteDays int
	warnedAt             *time.Time
}

type admin struct{ email, name string }

// Run sweeps shortly after start and then every interval until ctx ends.
func Run(ctx context.Context, d Deps, interval time.Duration) {
	t := time.NewTimer(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if r, err := Sweep(ctx, d); err != nil {
			if ctx.Err() == nil {
				d.Log.Warn().Err(err).Msg("dormant workspaces: sweep failed")
			}
		} else if r.Warned+r.Deleted > 0 {
			d.Log.Info().Int("warned", r.Warned).Int("deleted", r.Deleted).Msg("dormant workspaces")
		}
		t.Reset(interval)
	}
}

// Sweep tells the administrators of every workspace past its plan's warning
// period, and deletes every one past its deletion period whose notice has
// run its course.
func Sweep(ctx context.Context, d Deps) (Result, error) {
	var res Result
	if d.Mailer == nil {
		return res, nil
	}
	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	tx, err := d.Control.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var mine bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, sweepLock).Scan(&mine); err != nil || !mine {
		return res, err
	}

	warn, err := candidates(ctx, d.Control, `
		t.dormancy_warned_at IS NULL
		AND COALESCE(t.last_used_at, t.created_at) < $1::timestamptz - make_interval(days => p.dormant_warn_days)`, now())
	if err != nil {
		return res, fmt.Errorf("list workspaces to warn: %w", err)
	}
	for _, c := range warn {
		admins, err := tenantAdmins(ctx, d.Router, c.id)
		if err != nil {
			d.Log.Warn().Err(err).Str("tenant", c.id).Msg("dormant workspaces: read administrators")
			continue
		}
		deleteOn := c.lastUsed.AddDate(0, 0, c.deleteDays)
		if earliest := now().AddDate(0, 0, c.deleteDays-c.warnDays); deleteOn.Before(earliest) {
			deleteOn = earliest
		}
		subject, body := warningMail(d, c, deleteOn)
		if !mailAll(ctx, d, c, admins, subject, body) {
			continue // not told, so not counting down: tried again next sweep
		}
		if _, err := d.Control.Exec(ctx, `
			UPDATE platform.tenant_database SET dormancy_warned_at = $2
			WHERE customer_id = $1::uuid AND dormancy_warned_at IS NULL`, c.id, now()); err != nil {
			d.Log.Warn().Err(err).Str("tenant", c.id).Msg("dormant workspaces: record the notice")
			continue
		}
		res.Warned++
		d.Log.Info().Str("tenant", c.id).Str("name", c.name).Time("delete_on", deleteOn).Int("told", len(admins)).
			Msg("dormant workspace: administrators told it will be deleted")
	}

	del, err := candidates(ctx, d.Control, `
		t.dormancy_warned_at IS NOT NULL
		AND COALESCE(t.last_used_at, t.created_at) < $1::timestamptz - make_interval(days => p.dormant_delete_days)
		AND t.dormancy_warned_at <= $1::timestamptz - make_interval(days => p.dormant_delete_days - p.dormant_warn_days)`, now())
	if err != nil {
		return res, fmt.Errorf("list workspaces to delete: %w", err)
	}
	for _, c := range del {
		// Used in the meantime (the notice withdrawn)? Then it stays.
		var still bool
		if err := d.Control.QueryRow(ctx, `
			SELECT dormancy_warned_at IS NOT NULL FROM platform.tenant_database WHERE customer_id = $1::uuid`,
			c.id).Scan(&still); err != nil || !still {
			continue
		}
		admins, err := tenantAdmins(ctx, d.Router, c.id)
		if err != nil {
			d.Log.Warn().Err(err).Str("tenant", c.id).Msg("dormant workspaces: read administrators")
			continue
		}
		if err := d.Delete(ctx, c.id); err != nil {
			d.Log.Error().Err(err).Str("tenant", c.id).Msg("dormant workspaces: delete")
			continue
		}
		res.Deleted++
		d.Log.Info().Str("tenant", c.id).Str("name", c.name).Msg("dormant workspace deleted")
		subject, body := deletedMail(d, c, now())
		mailAll(ctx, d, c, admins, subject, body)
	}
	return res, tx.Commit(ctx)
}

func candidates(ctx context.Context, db *pgxpool.Pool, where string, now time.Time) ([]candidate, error) {
	rows, err := db.Query(ctx, `
		SELECT t.customer_id::text, t.name, p.name, COALESCE(t.last_used_at, t.created_at),
		       p.dormant_warn_days, p.dormant_delete_days, t.dormancy_warned_at
		FROM platform.tenant_database t JOIN platform.plan p ON p.key = t.plan
		WHERE t.status = 'ready' AND p.dormant_warn_days > 0 AND `+where+`
		ORDER BY 4`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.name, &c.plan, &c.lastUsed, &c.warnDays, &c.deleteDays, &c.warnedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// tenantAdmins are the people who administer the tenant and can be mailed.
func tenantAdmins(ctx context.Context, router *tenantdb.Router, customerID string) ([]admin, error) {
	if router == nil {
		return nil, errors.New("no tenant databases")
	}
	pool, err := router.Pool(ctx, customerID)
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT u.email, COALESCE(u.display_name, '') FROM identity.user u
		JOIN identity.role_assignment ra ON ra.user_id = u.id AND ra.role = 'tenant_admin'
		WHERE u.customer_id = $1::uuid AND u.email <> ''
		ORDER BY 1`, customerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []admin
	for rows.Next() {
		var a admin
		if err := rows.Scan(&a.email, &a.name); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// mailAll sends to every administrator and reports whether at least one was
// told. A tenant with no administrator to tell counts as told: nobody can be
// reached, and waiting would keep it forever.
func mailAll(ctx context.Context, d Deps, c candidate, admins []admin, subject, body string) bool {
	if len(admins) == 0 {
		d.Log.Warn().Str("tenant", c.id).Msg("dormant workspaces: no administrator with an address to tell")
		return true
	}
	told := false
	for _, a := range admins {
		if err := d.Mailer.Send(ctx, a.email, a.name, subject, body); err != nil {
			d.Log.Warn().Err(err).Str("tenant", c.id).Msg("dormant workspaces: mail an administrator")
			continue
		}
		told = true
	}
	return told
}

func host(consoleURL string) string {
	if u, err := url.Parse(consoleURL); err == nil && u.Host != "" {
		return u.Host
	}
	return "the platform"
}

func restoreLine(d Deps, by time.Time) string {
	if d.BackupRetentionDays <= 0 {
		return ""
	}
	if d.ContactEmail == "" {
		return fmt.Sprintf("Backups keep a copy for %d days after deletion.", d.BackupRetentionDays)
	}
	return fmt.Sprintf("Backups keep a copy for %d days after deletion: write to %s before %s to have it restored.",
		d.BackupRetentionDays, d.ContactEmail, by.UTC().Format("2 January 2006"))
}

func warningMail(d Deps, c candidate, deleteOn time.Time) (string, string) {
	date := deleteOn.UTC().Format("2 January 2006")
	subject := fmt.Sprintf("Your workspace %q will be deleted on %s", c.name, date)
	lines := []string{
		"Hello,",
		"",
		fmt.Sprintf("Nobody has used the workspace %q on %s for %d days. Workspaces in the Playground that are not used are deleted, so this one will be deleted on %s, with everything in it.",
			c.name, host(d.ConsoleURL), c.warnDays, date),
		"",
		"To keep it, sign in before then: " + strings.TrimRight(d.ConsoleURL, "/") + ". Any use by you or your colleagues keeps it.",
	}
	if r := restoreLine(d, deleteOn.AddDate(0, 0, d.BackupRetentionDays)); r != "" {
		lines = append(lines, "", r)
	}
	lines = append(lines, "", "This message went to the workspace's administrators.")
	return subject, strings.Join(lines, "\n")
}

func deletedMail(d Deps, c candidate, on time.Time) (string, string) {
	subject := fmt.Sprintf("Your workspace %q was deleted", c.name)
	lines := []string{
		"Hello,",
		"",
		fmt.Sprintf("The workspace %q on %s was deleted today, because nobody had used it for %d days, as announced to its administrators.",
			c.name, host(d.ConsoleURL), c.deleteDays),
	}
	if r := restoreLine(d, on.AddDate(0, 0, d.BackupRetentionDays)); r != "" {
		lines = append(lines, "", r)
	}
	return subject, strings.Join(lines, "\n")
}
