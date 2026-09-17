package main

import (
	"context"
	"fmt"
	"github.com/mavericks-engine/mavericks/ee/auditexport"
	"github.com/mavericks-engine/mavericks/ee/branding"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mavericks-engine/mavericks/internal/gateway"
	"github.com/mavericks-engine/mavericks/internal/identity"
	"github.com/mavericks-engine/mavericks/internal/notification"
	"github.com/mavericks-engine/mavericks/internal/plan"
	"github.com/mavericks-engine/mavericks/internal/workflow"
	"github.com/rs/zerolog"

	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/config"
	"github.com/mavericks-engine/mavericks/pkg/db"
	"github.com/mavericks-engine/mavericks/pkg/keycloak"
	"github.com/mavericks-engine/mavericks/pkg/license"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/migrate"
	"github.com/mavericks-engine/mavericks/pkg/objectstore"
	"github.com/mavericks-engine/mavericks/pkg/telemetry"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// schedulerPollInterval is how often this process checks for due scheduled
// automation rules; schedulerMisfireThreshold is how overdue a tick has to
// be before a rule's misfire_policy applies at all (a tick found only
// slightly late, within one normal poll, always fires regardless of
// policy — see internal/workflow/scheduler.go).
const (
	schedulerPollInterval = 30 * time.Second
	// How often a running gateway looks for tenants created since it
	// started, so their schedulers begin without a restart.
	tenantWatchInterval       = 30 * time.Second
	schedulerMisfireThreshold = 5 * time.Minute
	// How often outbound notifications are delivered, and how often overdue
	// tasks are looked for. A reminder is not urgent to the minute, and each
	// pass is one indexed query per database.
	notificationDispatchInterval = 20 * time.Second
	reminderInterval             = 5 * time.Minute
	auditRetentionInterval       = 24 * time.Hour
	// How often every tenant is recounted against its plan's limits.
	planSweepInterval = 5 * time.Minute
)

type cfg struct {
	config.BaseConfig `mapstructure:",squash"`
	HTTPPort          int    `mapstructure:"HTTP_PORT"`
	OTelEndpoint      string `mapstructure:"OTEL_EXPORTER_OTLP_ENDPOINT"`
	ServiceVersion    string `mapstructure:"SERVICE_VERSION"`
	// Object storage backs exactly one feature (standalone deployment
	// package export) as of this item — gateway-only config, not
	// pkg/config.BaseConfig, since every other cmd/* binary has no need
	// for it. MINIO_URL/MINIO_ROOT_USER/MINIO_ROOT_PASSWORD are already
	// provisioned in deploy/k8s/base/configmap.yaml and secrets.yaml;
	// OBJECT_STORE_BUCKET is the one genuinely new key.
	ObjectStoreURL       string `mapstructure:"MINIO_URL"`
	ObjectStoreAccessKey string `mapstructure:"MINIO_ROOT_USER"`
	ObjectStoreSecretKey string `mapstructure:"MINIO_ROOT_PASSWORD"`
	ObjectStoreBucket    string `mapstructure:"OBJECT_STORE_BUCKET"`
	// License key for the commercial/enterprise editions (pkg/license). The
	// key itself, or a file holding it; empty runs the community edition.
	// LicensePublicKey overrides the compiled-in verifier key (tests, forks).
	LicenseKey       string `mapstructure:"MAVERICKS_LICENSE_KEY"`
	LicenseFile      string `mapstructure:"MAVERICKS_LICENSE_FILE"`
	LicensePublicKey string `mapstructure:"MAVERICKS_LICENSE_PUBLIC_KEY"`
	// TenantDBMode: "shared" (default) keeps every tenant in the database
	// DATABASE_URL points at; "dedicated" gives each tenant its own database
	// on the same server and makes DATABASE_URL the control plane. See
	// docs/TENANT_DATABASES.md.
	TenantDBMode string `mapstructure:"TENANT_DB_MODE"`
	// TenantDBAdminURL is a connection string with CREATEDB rights, used only
	// to create and drop tenant databases. Empty reuses DATABASE_URL's
	// credentials, which is correct when the application role owns the server.
	TenantDBAdminURL string `mapstructure:"TENANT_DB_ADMIN_URL"`
	// TenantDBMaxConns caps each tenant's pool (default 5): hundreds of
	// tenants must not each hold a full-sized pool open.
	TenantDBMaxConns int `mapstructure:"TENANT_DB_MAX_CONNS"`
	// Outbound e-mail. SMTPHost empty means no relay: e-mail notifications
	// are then reported as undeliverable instead of queueing forever.
	SMTPHost     string `mapstructure:"SMTP_HOST"`
	SMTPPort     int    `mapstructure:"SMTP_PORT"`
	SMTPUsername string `mapstructure:"SMTP_USERNAME"`
	SMTPPassword string `mapstructure:"SMTP_PASSWORD"`
	SMTPFrom     string `mapstructure:"SMTP_FROM"`
	// Self-service sign-up (docs/PLANS_AND_TRIALS.md): off unless a
	// deployment opts in. PlanContactURL is where "change the plan" leads
	// in every trial notice and plan-limit refusal.
	SignupEnabled  bool   `mapstructure:"SIGNUP_ENABLED"`
	PlanContactURL string `mapstructure:"PLAN_CONTACT_URL"`
}

func main() {
	log := logger.New("gateway")
	ctx := context.Background()

	var c cfg
	c.HTTPPort = 8080
	c.TenantDBMaxConns = 5
	c.SMTPPort = 587
	c.KeycloakURL = "http://localhost:8180"
	c.KeycloakRealm = "mavericks"
	c.ObjectStoreURL = "http://localhost:9000"
	c.ObjectStoreAccessKey = "mavericks"
	c.ObjectStoreSecretKey = "mavericks123"
	c.ObjectStoreBucket = "mavericks"
	if err := config.Load(&c); err != nil {
		log.Fatal().Err(err).Msg("failed to load config")
		os.Exit(1)
	}

	if c.DatabaseURL == "" {
		c.DatabaseURL = "postgres://mavericks:mavericks@localhost:5432/mavericks?sslmode=disable"
	}
	if c.OTelEndpoint == "" {
		c.OTelEndpoint = "localhost:4317"
	}

	// Initialise OpenTelemetry (no-ops gracefully if collector is unreachable)
	shutdown, err := telemetry.Setup(ctx, telemetry.Config{
		ServiceName:       "gateway",
		ServiceVersion:    c.ServiceVersion,
		CollectorEndpoint: c.OTelEndpoint,
	})
	if err != nil {
		log.Warn().Err(err).Msg("telemetry setup failed — continuing without OTel")
	} else {
		defer func() {
			if err := shutdown(context.Background()); err != nil {
				log.Error().Err(err).Msg("telemetry shutdown error")
			}
		}()
		log.Info().Str("endpoint", c.OTelEndpoint).Msg("OpenTelemetry initialised")
	}

	pool, err := db.Connect(ctx, c.DatabaseURL)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to database")
		os.Exit(1)
	}

	if err := migrate.Run(ctx, pool, migrationfs.FS, "."); err != nil {
		log.Fatal().Err(err).Msg("migration failed")
		os.Exit(1)
	}
	log.Info().Msg("migrations applied")
	defer pool.Close()

	// Outside DEV_MODE, a working JWKS validator is mandatory: resolveActor
	// refuses every request when h.jwks is nil rather than falling back to
	// X-Dev-User persona auth (see internal/gateway/handler.go), so refusing
	// to start here is what actually enforces that — a silently-nil
	// validator in a running process would just mean every request 401s.
	devMode := os.Getenv("DEV_MODE") == "true"
	var jwks *identity.JWKSValidator
	if devMode {
		log.Warn().Msg("DEV_MODE=true: JWKS validation disabled, X-Dev-User persona auth is active")
	} else {
		jwks, err = identity.NewJWKSValidator(ctx, c.KeycloakURL, c.KeycloakRealm, c.KeycloakIssuer)
		if err != nil {
			log.Fatal().Err(err).Msg("failed to initialize JWKS validator")
			os.Exit(1)
		}
	}

	// Object storage backs standalone deployment package export. Client
	// construction never dials the network (minio.New only validates the
	// URL/credential shape), so a failure here means genuinely broken
	// config, not transient unavailability — still non-fatal: the rest of
	// the gateway has nothing to do with this feature, so store stays nil
	// and the one handler that needs it fails loudly per-request instead of
	// refusing to start the whole process. EnsureBucket, by contrast, DOES
	// touch the network — best-effort at boot (warn, continue), mirroring
	// the OTel setup above, since MinIO being briefly unreachable at boot
	// shouldn't crash-loop every other route.
	var objStore *objectstore.Store
	endpoint, useSSL, err := parseObjectStoreEndpoint(c.ObjectStoreURL)
	if err != nil {
		log.Warn().Err(err).Str("url", c.ObjectStoreURL).Msg("invalid object store URL — package export will be unavailable")
	} else if objClient, err := objectstore.NewClient(endpoint, c.ObjectStoreAccessKey, c.ObjectStoreSecretKey, c.ObjectStoreBucket, useSSL); err != nil {
		log.Warn().Err(err).Msg("object store client construction failed — package export will be unavailable")
	} else {
		if err := objClient.EnsureBucket(ctx); err != nil {
			log.Warn().Err(err).Msg("object store bucket setup failed — continuing, package export may fail until it's reachable")
		}
		objStore = objectstore.NewStore(objClient, objectstore.NewMetaStore(pool))
	}

	// Keycloak admin client, used to provision real accounts for users created
	// in the console. Constructing it dials nothing, so a bad secret surfaces
	// on first use rather than here.
	//
	// Absent configuration is correct in dev, where X-Dev-User personas stand
	// in for real accounts. Outside dev it is a misconfiguration, but not a
	// fatal one: it disables exactly one route, which then fails loudly with
	// an actionable message. Refusing to start would take the whole API down
	// over user administration.
	var kc *keycloak.Client
	if c.KeycloakAdminClientID != "" && c.KeycloakAdminClientSecret != "" {
		kc = keycloak.New(c.KeycloakURL, c.KeycloakRealm,
			c.KeycloakAdminClientID, c.KeycloakAdminClientSecret, c.ConsoleURL)
		log.Info().Str("client_id", c.KeycloakAdminClientID).Msg("Keycloak user provisioning enabled")
	} else if !devMode {
		log.Warn().Msg("KEYCLOAK_ADMIN_CLIENT_ID/SECRET unset: creating users through the console " +
			"will be refused, because the accounts it made could not sign in")
	}

	// Wrap the handler with OTel HTTP instrumentation (traces + metrics per route)
	// Tenant databases. In shared mode router stays nil and everything below
	// behaves exactly as it did before dedicated databases existed.
	var router *tenantdb.Router
	if strings.EqualFold(c.TenantDBMode, "dedicated") {
		router = tenantdb.New(pool, tenantdb.Config{
			MaxConnsPerTenant: int32(c.TenantDBMaxConns), //nolint:gosec // a pool size, bounded by config
			Migrations:        migrationfs.FS,
			MigrationsDir:     ".",
			AdminURL:          c.TenantDBAdminURL,
			Log:               log,
		})
		defer router.Close()
		// Every tenant database carries the same schema, so a deploy has to
		// upgrade all of them before serving. A tenant whose migration fails
		// is marked failed and refused rather than served at the wrong
		// schema; the rest start normally.
		if err := router.MigrateAll(ctx); err != nil {
			log.Error().Err(err).Msg("one or more tenant databases could not be migrated — they are disabled until fixed")
		}
		tenants, _ := router.Catalog().List(ctx)
		log.Info().Int("tenants", len(tenants)).Msg("dedicated tenant databases enabled")
	} else {
		log.Info().Msg("shared tenant database (TENANT_DB_MODE=dedicated gives each tenant its own)")
	}

	mailer := notification.NewMailer(notification.SMTPConfig{
		Host: c.SMTPHost, Port: c.SMTPPort, Username: c.SMTPUsername,
		Password: c.SMTPPassword, From: c.SMTPFrom,
	})
	if mailer != nil {
		log.Info().Str("host", c.SMTPHost).Int("port", c.SMTPPort).Str("from", c.SMTPFrom).Msg("outbound e-mail enabled")
	} else {
		log.Info().Msg("no SMTP relay configured (SMTP_HOST): e-mail notifications will not be delivered")
	}

	lic := license.Load(license.Options{Key: c.LicenseKey, File: c.LicenseFile, PublicKey: c.LicensePublicKey})
	switch st := lic.Status(); st.State {
	case license.StateActive:
		log.Info().Str("edition", string(st.Edition)).Str("customer", st.Customer).Time("expires_at", *st.ExpiresAt).Msg("license key accepted")
	case license.StateExpired:
		log.Warn().Str("customer", st.Customer).Time("expired_at", *st.ExpiresAt).Msg("license key has EXPIRED — running the community edition until a new key is installed")
	case license.StateInvalid:
		log.Error().Str("source", st.Source).Str("reason", st.Error).Msg("license key could not be verified — running the community edition")
	default:
		log.Info().Msg("no license key configured — running the community edition")
	}

	// Plans are read from the control plane. One enforcer serves both the
	// requests and the sweep below, so a sweep's verdict applies at once.
	planEnforcer := plan.NewEnforcer(pool)
	handler := otelhttp.NewHandler(
		gateway.NewHandlerWithDeps(log, pool, jwks, gateway.Deps{
			Plans:       planEnforcer,
			ObjectStore: objStore,
			Keycloak:    kc,
			License:     lic,
			Router:      router,
			Signup:      gateway.SignupConfig{Enabled: c.SignupEnabled, ContactURL: c.PlanContactURL},

			MailerConfigured:  mailer != nil,
			KeycloakPublicURL: firstNonEmptyString(c.KeycloakIssuer, c.KeycloakURL), KeycloakRealm: c.KeycloakRealm, PublicURL: c.ConsoleURL,
		}),
		"gateway",
		otelhttp.WithMessageEvents(otelhttp.ReadEvents, otelhttp.WriteEvents),
	)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", c.HTTPPort),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Scheduled automation: a ticker goroutine per gateway process, safe to
	// run from every replica simultaneously — workflow.execution's partial
	// unique index on (rule_id, scheduled_for) makes firing a given tick
	// idempotent regardless of how many instances poll for it at once (see
	// internal/workflow/scheduler.go).
	schedCtx, cancelScheduler := context.WithCancel(ctx)
	defer cancelScheduler()
	hostname, _ := os.Hostname()
	workerID := fmt.Sprintf("%s-%d", hostname, os.Getpid())
	// One scheduler per database: workflow timers live with the data they
	// fire against. In shared mode that is the single pool; in dedicated mode
	// every tenant gets its own, including tenants created while running.
	// startBackground runs the loops that belong to one database: workflow
	// timers, outbound notification delivery, task reminders and the plan
	// sweep.
	startBackground := func(dbPool *pgxpool.Pool, blog zerolog.Logger, id string) {
		go workflow.RunScheduler(schedCtx, workflow.NewStore(dbPool), blog,
			schedulerPollInterval, schedulerMisfireThreshold, id)
		store := notification.NewStore(dbPool)
		dispatcher := &notification.Dispatcher{Store: store, Mailer: mailer, Log: blog,
			// A white-labelled tenant's mail carries its own name; on other
			// editions the name is empty and the platform's own is used.
			BrandName: func(ctx context.Context, recipientUserID string) string {
				if !lic.Has(license.FeatureWhiteLabel) {
					return ""
				}
				return branding.EmailNameForUser(ctx, dbPool, recipientUserID)
			}}
		go dispatcher.Run(schedCtx, notificationDispatchInterval)
		reminder := &notification.Reminder{Store: store, Log: blog}
		go reminder.Run(schedCtx, reminderInterval)
		// Enterprise audit retention: sweeps only while the licence includes
		// it — keeping events is the safe failure when a key lapses.
		go auditexport.RunRetention(schedCtx, dbPool, blog, auditRetentionInterval, func() bool { return lic.Has(license.FeatureAuditExport) })
		// Plan limits: a tenant over its plan is marked read-only here,
		// whichever path put it over (internal/plan).
		go plan.RunSweep(schedCtx, dbPool, planEnforcer, planSweepInterval, blog)
	}
	if router != nil {
		router.Watch(schedCtx, tenantWatchInterval, func(t tenantdb.Tenant, tpool *pgxpool.Pool) {
			startBackground(tpool, log.With().Str("tenant", t.CustomerID).Logger(), workerID+"-"+t.CustomerID[:8])
		})
	} else {
		startBackground(pool, log, workerID)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		log.Info().Int("port", c.HTTPPort).Msg("HTTP gateway listening")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("gateway error")
		}
	}()

	<-stop
	log.Info().Msg("shutting down gateway")
	cancelScheduler()
	if err := srv.Shutdown(context.Background()); err != nil {
		log.Error().Err(err).Msg("shutdown error")
	}
}

// parseObjectStoreEndpoint splits a "http(s)://host:port" URL (the shape
// MINIO_URL is always configured as, in both docker-compose.dev.yml and
// deploy/k8s/base/configmap.yaml) into the bare host:port minio-go/v7
// expects plus whether TLS should be used.
func parseObjectStoreEndpoint(rawURL string) (endpoint string, useSSL bool, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", false, fmt.Errorf("parse object store URL: %w", err)
	}
	if u.Host == "" {
		return "", false, fmt.Errorf("object store URL %q has no host", rawURL)
	}
	return u.Host, u.Scheme == "https", nil
}

func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
