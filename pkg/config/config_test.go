package config

import (
	"testing"

	"github.com/spf13/viper"
)

// Every test resets viper's global state — Load/BindEnv/SetDefault all
// write into the package-level singleton, so a prior test's bindings would
// otherwise leak into the next one.
func resetViper(t *testing.T) {
	t.Helper()
	viper.Reset()
}

func TestLoad_DatabaseURLFromEnv(t *testing.T) {
	resetViper(t)
	t.Setenv("DATABASE_URL", "postgres://from-env/db")

	type cfg struct {
		BaseConfig `mapstructure:",squash"`
	}
	var c cfg
	if err := Load(&c); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DatabaseURL != "postgres://from-env/db" {
		t.Errorf("DatabaseURL = %q, want the env var value — a field with no viper.SetDefault must still bind from its env var", c.DatabaseURL)
	}
}

func TestLoad_EmbeddedBaseConfigMustBeSquashed(t *testing.T) {
	resetViper(t)

	type cfg struct {
		BaseConfig `mapstructure:",squash"`
		Extra      string `mapstructure:"EXTRA"`
	}
	var c cfg
	if err := Load(&c); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.GRPCPort != 9090 {
		t.Errorf("GRPCPort = %d, want 9090 (the registered default) — without the `mapstructure:\",squash\"` tag on the embedded BaseConfig, mapstructure never flattens its fields and Unmarshal silently leaves them at the zero value", c.GRPCPort)
	}
}

func TestLoad_PreSetGoDefaultSurvivesWhenNoEnvOverride(t *testing.T) {
	resetViper(t)

	type cfg struct {
		BaseConfig  `mapstructure:",squash"`
		KeycloakURL string `mapstructure:"KEYCLOAK_URL"`
	}
	var c cfg
	c.KeycloakURL = "http://preset-default"
	if err := Load(&c); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.KeycloakURL != "http://preset-default" {
		t.Errorf("KeycloakURL = %q, want the pre-set Go default to survive when no env var overrides it", c.KeycloakURL)
	}
}

func TestLoad_EnvOverridesPreSetGoDefault(t *testing.T) {
	resetViper(t)
	t.Setenv("KEYCLOAK_URL", "http://from-env:8180")

	type cfg struct {
		BaseConfig  `mapstructure:",squash"`
		KeycloakURL string `mapstructure:"KEYCLOAK_URL"`
	}
	var c cfg
	c.KeycloakURL = "http://preset-default"
	if err := Load(&c); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.KeycloakURL != "http://from-env:8180" {
		t.Errorf("KeycloakURL = %q, want the env var to override the pre-set Go default", c.KeycloakURL)
	}
}
