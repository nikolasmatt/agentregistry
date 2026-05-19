package config

import (
	"os"
	"strings"
	"testing"
)

func TestNewConfig_RuntimeDirHasRandomSuffix(t *testing.T) {
	// Ensure the env var is unset so the default path is used.
	os.Unsetenv("AGENT_REGISTRY_RUNTIME_DIR")

	cfg := NewConfig()

	base := "/tmp/arctl-runtime-"
	if !strings.HasPrefix(cfg.RuntimeDir, base) {
		t.Fatalf("RuntimeDir should start with %q, got %q", base, cfg.RuntimeDir)
	}

	suffix := strings.TrimPrefix(cfg.RuntimeDir, base)
	if len(suffix) != 16 { // 8 bytes = 16 hex chars
		t.Fatalf("RuntimeDir suffix should be 16 hex chars, got %q (len %d)", suffix, len(suffix))
	}
}

func TestNewConfig_RuntimeDirUniqueBetweenCalls(t *testing.T) {
	os.Unsetenv("AGENT_REGISTRY_RUNTIME_DIR")

	cfg1 := NewConfig()
	cfg2 := NewConfig()

	if cfg1.RuntimeDir == cfg2.RuntimeDir {
		t.Fatalf("two NewConfig() calls should produce different RuntimeDir values, both got %q", cfg1.RuntimeDir)
	}
}

func TestNewConfig_RuntimeDirRespectsEnvOverride(t *testing.T) {
	custom := "/custom/runtime/path"
	t.Setenv("AGENT_REGISTRY_RUNTIME_DIR", custom)

	cfg := NewConfig()

	if cfg.RuntimeDir != custom {
		t.Fatalf("RuntimeDir should be %q when env var is set, got %q", custom, cfg.RuntimeDir)
	}
}

func TestNewConfig_SkipMigrationsEnv(t *testing.T) {
	cases := []struct {
		name     string
		prefixed string // value of AGENT_REGISTRY_SKIP_MIGRATIONS; "" means unset
		bare     string // value of SKIP_MIGRATIONS; "" means unset
		want     bool
	}{
		{"both unset", "", "", false},
		{"prefixed true", "true", "", true},
		{"bare true", "", "true", true},
		{"bare false explicit", "", "false", false},
		{"prefixed wins over bare", "true", "false", true},
		{"prefixed false wins over bare true", "false", "true", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			os.Unsetenv("AGENT_REGISTRY_SKIP_MIGRATIONS")
			os.Unsetenv("SKIP_MIGRATIONS")
			if tc.prefixed != "" {
				t.Setenv("AGENT_REGISTRY_SKIP_MIGRATIONS", tc.prefixed)
			}
			if tc.bare != "" {
				t.Setenv("SKIP_MIGRATIONS", tc.bare)
			}
			cfg := NewConfig()
			if cfg.SkipMigrations != tc.want {
				t.Fatalf("SkipMigrations = %v; want %v (prefixed=%q bare=%q)", cfg.SkipMigrations, tc.want, tc.prefixed, tc.bare)
			}
		})
	}
}
