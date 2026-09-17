// Package branding is white-labelling: a tenant's own name, logo, favicon,
// colour and tagline on the console, its name on outbound e-mail, and a
// custom host that shows the tenant's brand before anyone signs in.
// Licensed under ee/LICENSE; gated by license.FeatureWhiteLabel, which
// commercial and enterprise editions include.
//
// Images are stored inline as data URLs with hard size limits, so a brand
// travels in one small request and needs no object store.
package branding

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Settings is the one row of core.branding.
type Settings struct {
	ProductName    string `json:"product_name"`
	Tagline        string `json:"tagline"`
	LogoDataURL    string `json:"logo_data_url"`
	FaviconDataURL string `json:"favicon_data_url"`
	BrandColor     string `json:"brand_color"`
	EmailFromName  string `json:"email_from_name"`
	CustomDomain   string `json:"custom_domain"`
	// Configured is true when anything is set: the console applies a brand
	// only then, and keeps its own look otherwise.
	Configured bool      `json:"configured"`
	UpdatedAt  time.Time `json:"updated_at"`
}

const (
	MaxLogoBytes    = 256 * 1024
	MaxFaviconBytes = 32 * 1024
	MaxNameLen      = 60
	MaxTaglineLen   = 120
)

var (
	colorRe  = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	domainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
)

// allowedImageTypes are what a browser tab and a sidebar can show.
var allowedImageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/svg+xml": true, "image/webp": true, "image/x-icon": true, "image/vnd.microsoft.icon": true}

// Store reads and writes one tenant's row. It is keyed by customer even
// inside a dedicated database: on a shared database that is what keeps one
// tenant's brand from being everyone's.
type Store struct {
	pool       *pgxpool.Pool
	customerID string
}

func NewStore(pool *pgxpool.Pool, customerID string) *Store {
	return &Store{pool: pool, customerID: customerID}
}

func (s Settings) configured() bool {
	return s.ProductName != "" || s.Tagline != "" || s.LogoDataURL != "" || s.FaviconDataURL != "" || s.BrandColor != "" || s.EmailFromName != "" || s.CustomDomain != ""
}

// Get returns the row; a database that predates it reports "not configured".
func (st *Store) Get(ctx context.Context) (Settings, error) {
	var s Settings
	err := st.pool.QueryRow(ctx, `
		SELECT product_name, tagline, logo_data_url, favicon_data_url, brand_color, email_from_name, custom_domain, updated_at
		FROM core.branding WHERE customer_id = $1::uuid
	`, st.customerID).Scan(&s.ProductName, &s.Tagline, &s.LogoDataURL, &s.FaviconDataURL, &s.BrandColor, &s.EmailFromName, &s.CustomDomain, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Settings{}, nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("read branding: %w", err)
	}
	s.Configured = s.configured()
	return s, nil
}

// Validate normalises and checks what a tenant admin submitted.
func Validate(in Settings) (Settings, error) {
	in.ProductName = strings.TrimSpace(in.ProductName)
	in.Tagline = strings.TrimSpace(in.Tagline)
	in.EmailFromName = strings.TrimSpace(in.EmailFromName)
	in.BrandColor = strings.TrimSpace(in.BrandColor)
	if len(in.ProductName) > MaxNameLen {
		return Settings{}, fmt.Errorf("product name must be at most %d characters", MaxNameLen)
	}
	if len(in.Tagline) > MaxTaglineLen {
		return Settings{}, fmt.Errorf("tagline must be at most %d characters", MaxTaglineLen)
	}
	if len(in.EmailFromName) > MaxNameLen {
		return Settings{}, fmt.Errorf("e-mail sender name must be at most %d characters", MaxNameLen)
	}
	if in.BrandColor != "" && !colorRe.MatchString(in.BrandColor) {
		return Settings{}, fmt.Errorf("brand colour must be a hex colour like #4f46e5")
	}
	if in.BrandColor != "" {
		in.BrandColor = strings.ToLower(in.BrandColor)
	}
	if err := checkImage("logo", in.LogoDataURL, MaxLogoBytes); err != nil {
		return Settings{}, err
	}
	if err := checkImage("favicon", in.FaviconDataURL, MaxFaviconBytes); err != nil {
		return Settings{}, err
	}
	dom, err := NormalizeDomain(in.CustomDomain)
	if err != nil {
		return Settings{}, err
	}
	in.CustomDomain = dom
	in.Configured = in.configured()
	return in, nil
}

// checkImage accepts an empty value or a data URL of an allowed image type
// within the byte limit.
func checkImage(what, dataURL string, limit int) error {
	if dataURL == "" {
		return nil
	}
	if !strings.HasPrefix(dataURL, "data:") {
		return fmt.Errorf("%s must be an image data URL", what)
	}
	meta, payload, ok := strings.Cut(dataURL[len("data:"):], ",")
	if !ok || !strings.HasSuffix(meta, ";base64") {
		return fmt.Errorf("%s must be a base64 image data URL", what)
	}
	typ := strings.TrimSuffix(meta, ";base64")
	if !allowedImageTypes[typ] {
		return fmt.Errorf("%s must be a PNG, JPEG, SVG, WebP or ICO image", what)
	}
	if n := base64.StdEncoding.DecodedLen(len(payload)); n > limit {
		return fmt.Errorf("%s must be at most %d KB", what, limit/1024)
	}
	if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
		return fmt.Errorf("%s is not valid base64", what)
	}
	return nil
}

// NormalizeDomain lower-cases a host and strips what people paste with it
// (a scheme, a port, a path). Empty stays empty.
func NormalizeDomain(d string) (string, error) {
	d = strings.ToLower(strings.TrimSpace(d))
	if d == "" {
		return "", nil
	}
	if i := strings.Index(d, "://"); i >= 0 {
		d = d[i+3:]
	}
	if i := strings.IndexAny(d, "/?#"); i >= 0 {
		d = d[:i]
	}
	if i := strings.Index(d, ":"); i >= 0 {
		d = d[:i]
	}
	if !domainRe.MatchString(d) {
		return "", fmt.Errorf("custom domain must be a host name like planning.example.com")
	}
	return d, nil
}

// Update validates and writes the row.
func (st *Store) Update(ctx context.Context, in Settings) (Settings, error) {
	in, err := Validate(in)
	if err != nil {
		return Settings{}, err
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO core.branding (customer_id, product_name, tagline, logo_data_url, favicon_data_url, brand_color, email_from_name, custom_domain)
		VALUES ($8::uuid, $1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (customer_id) DO UPDATE SET product_name = $1, tagline = $2, logo_data_url = $3, favicon_data_url = $4,
		    brand_color = $5, email_from_name = $6, custom_domain = $7, updated_at = now()
	`, in.ProductName, in.Tagline, in.LogoDataURL, in.FaviconDataURL, in.BrandColor, in.EmailFromName, in.CustomDomain, st.customerID); err != nil {
		return Settings{}, fmt.Errorf("save branding: %w", err)
	}
	return st.Get(ctx)
}

// Clear returns the tenant to the platform's own look.
func (st *Store) Clear(ctx context.Context) (Settings, error) {
	if _, err := st.pool.Exec(ctx, `DELETE FROM core.branding WHERE customer_id = $1::uuid`, st.customerID); err != nil {
		return Settings{}, fmt.Errorf("clear branding: %w", err)
	}
	return st.Get(ctx)
}

// EmailName is what outbound mail is sent as: the sender name, else the
// product name, else empty (the platform's default).
func EmailName(ctx context.Context, pool *pgxpool.Pool, customerID string) string {
	if customerID == "" {
		return ""
	}
	s, err := NewStore(pool, customerID).Get(ctx)
	if err != nil {
		return ""
	}
	if s.EmailFromName != "" {
		return s.EmailFromName
	}
	return s.ProductName
}

// Domains is the control-plane index from a custom host to its tenant.
type Domains struct{ control *pgxpool.Pool }

func NewDomains(control *pgxpool.Pool) *Domains { return &Domains{control: control} }

// Set registers a tenant's host (or removes it when empty). A host another
// tenant already claims is refused.
func (d *Domains) Set(ctx context.Context, customerID, domain string) error {
	tx, err := d.control.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if domain != "" {
		var owner string
		if err := tx.QueryRow(ctx, `SELECT customer_id::text FROM platform.branding_domain WHERE domain = $1`, domain).Scan(&owner); err == nil && owner != customerID {
			return fmt.Errorf("the domain %s is already registered by another tenant", domain)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM platform.branding_domain WHERE customer_id = $1::uuid`, customerID); err != nil {
		return err
	}
	if domain != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO platform.branding_domain (domain, customer_id) VALUES ($1, $2::uuid)`, domain, customerID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Lookup finds the tenant a host belongs to, or "".
func (d *Domains) Lookup(ctx context.Context, host string) (string, error) {
	host, err := NormalizeDomain(host)
	if err != nil || host == "" {
		return "", nil
	}
	var customerID string
	err = d.control.QueryRow(ctx, `SELECT customer_id::text FROM platform.branding_domain WHERE domain = $1`, host).Scan(&customerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return customerID, err
}

// EmailNameForUser is EmailName for a recipient: the tenant is the user's
// customer, else the customer of the first workspace they hold a role in.
// The notification dispatcher runs per database and knows only the
// recipient, so this is how a white-labelled tenant's mail finds its name.
func EmailNameForUser(ctx context.Context, pool *pgxpool.Pool, userID string) string {
	if userID == "" {
		return ""
	}
	var customerID string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(u.customer_id::text, (
		    SELECT w.customer_id::text FROM identity.role_assignment ra JOIN core.workspace w ON w.id = ra.workspace_id
		    WHERE ra.user_id = u.id ORDER BY ra.assigned_at LIMIT 1), '')
		FROM identity."user" u WHERE u.id = $1::uuid`, userID).Scan(&customerID); err != nil {
		return ""
	}
	return EmailName(ctx, pool, customerID)
}
