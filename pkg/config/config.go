package config

import (
	"fmt"
	"reflect"

	"github.com/spf13/viper"
)

type BaseConfig struct {
	GRPCPort    int    `mapstructure:"GRPC_PORT"`
	MetricsPort int    `mapstructure:"METRICS_PORT"`
	LogLevel    string `mapstructure:"LOG_LEVEL"`
	DatabaseURL string `mapstructure:"DATABASE_URL"`
	NATSUrl     string `mapstructure:"NATS_URL"`
	RedisURL    string `mapstructure:"REDIS_URL"`
	// KeycloakURL/KeycloakRealm: every gRPC service's AuthInterceptor
	// needs these to build a production-mode JWKS validator (see
	// pkg/grpcutil.WithAuth), not just cmd/gateway — like DatabaseURL,
	// each service sets its own literal default in main() before
	// config.Load runs rather than registering one centrally here.
	KeycloakURL   string `mapstructure:"KEYCLOAK_URL"`
	KeycloakRealm string `mapstructure:"KEYCLOAK_REALM"`
	// KeycloakIssuer is Keycloak's PUBLIC origin — the one browsers use and
	// the one it stamps into every token's `iss` claim. It differs from
	// KeycloakURL whenever Keycloak is behind an ingress, so it is validated
	// separately from the address used to fetch JWKS. Empty means "same as
	// KeycloakURL", which is correct for the dev stack. See
	// internal/identity.NewJWKSValidator.
	KeycloakIssuer string `mapstructure:"KEYCLOAK_ISSUER"`
	// Service-account credentials the gateway uses to provision users in
	// Keycloak (see pkg/keycloak). Scoped to manage-users/view-users on one
	// realm — deliberately NOT the master realm administrator, whose
	// credentials would give a compromised gateway every realm on the server.
	// Empty disables provisioning, which is the correct state for the dev
	// stack, where X-Dev-User personas stand in for real accounts.
	KeycloakAdminClientID     string `mapstructure:"KEYCLOAK_ADMIN_CLIENT_ID"`
	KeycloakAdminClientSecret string `mapstructure:"KEYCLOAK_ADMIN_CLIENT_SECRET"`
	// ConsoleURL is where an invitation link should land after the recipient
	// sets their password. Also the origin the console is served from.
	ConsoleURL string `mapstructure:"CONSOLE_URL"`
}

func Load(cfg any) error {
	viper.AutomaticEnv()
	viper.SetDefault("GRPC_PORT", 9090)
	viper.SetDefault("METRICS_PORT", 9091)
	viper.SetDefault("LOG_LEVEL", "info")
	viper.SetDefault("NATS_URL", "nats://localhost:4222")
	viper.SetDefault("REDIS_URL", "redis://localhost:6379")

	// viper.AutomaticEnv() only makes an env var visible to Unmarshal for
	// keys viper already knows about (registered via SetDefault/BindEnv/
	// Set) — any mapstructure-tagged field without one of those, like
	// DatabaseURL above, is silently left at its zero value by Unmarshal
	// even when the matching env var IS set in the process environment
	// (see spf13/viper#188). Bind every mapstructure tag found on cfg
	// (including embedded structs such as BaseConfig itself) so every
	// field an env var can be provided for actually gets it, without every
	// caller having to remember to register one for each field it adds.
	if err := bindEnvTags(cfg); err != nil {
		return fmt.Errorf("bind env tags: %w", err)
	}

	if err := viper.Unmarshal(cfg); err != nil {
		return fmt.Errorf("config unmarshal: %w", err)
	}
	return nil
}

func bindEnvTags(cfg any) error {
	v := reflect.ValueOf(cfg)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return fmt.Errorf("cfg must be a struct or pointer to struct, got %s", v.Kind())
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.Anonymous && field.Type.Kind() == reflect.Struct {
			if err := bindEnvTags(v.Field(i).Addr().Interface()); err != nil {
				return err
			}
			continue
		}
		tag := field.Tag.Get("mapstructure")
		if tag == "" || tag == "-" {
			continue
		}
		if err := viper.BindEnv(tag); err != nil {
			return err
		}
	}
	return nil
}
