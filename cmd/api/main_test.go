package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
	"github.com/disaster/dagger-kubernetes/internal/observ"
	"github.com/disaster/dagger-kubernetes/internal/repository"
)

func newMetaStore(t *testing.T) *repository.MetaStore {
	t.Helper()
	db, err := repository.OpenSQLite(t.TempDir() + "/jwt.db")
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if err := repository.Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return repository.NewMetaStore(db)
}

func TestLoadOrCreateJWTSecretConfiguredOK(t *testing.T) {
	ms := newMetaStore(t)
	secret := strings.Repeat("k", minJWTSecretLen)
	got, err := loadOrCreateJWTSecret(context.Background(), ms, secret, observ.NewTestLogger())
	if err != nil {
		t.Fatalf("loadOrCreateJWTSecret: %v", err)
	}
	if string(got) != secret {
		t.Fatalf("secret = %q, want configured", got)
	}
}

func TestLoadOrCreateJWTSecretTooShortRejected(t *testing.T) {
	ms := newMetaStore(t)
	if _, err := loadOrCreateJWTSecret(context.Background(), ms, "short", observ.NewTestLogger()); err == nil {
		t.Fatal("expected error for short secret (CWE-326)")
	}
}

func TestLoadOrCreateJWTSecretGeneratedAndPersisted(t *testing.T) {
	ms := newMetaStore(t)
	logger := observ.NewTestLogger()

	got, err := loadOrCreateJWTSecret(context.Background(), ms, "", logger)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(got) < minJWTSecretLen {
		t.Fatalf("generated secret too short: %d", len(got))
	}

	// Second call returns the persisted value (stable across restarts).
	again, err := loadOrCreateJWTSecret(context.Background(), ms, "", logger)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !bytes.Equal(again, got) {
		t.Fatal("generated secret must be persisted and reused")
	}
}

func TestValidateFleetEnv(t *testing.T) {
	tests := []struct {
		name    string
		fleet   domain.FleetConfig
		wantErr bool
	}{
		{
			name: "valid proxy map",
			fleet: domain.FleetConfig{
				EngineExtraEnv: map[string]string{
					"HTTP_PROXY": "http://proxy.corp.example:3128",
				},
			},
			wantErr: false,
		},
		{
			name: "DAGGER_CACHE_TOKEN in engine_extra_env",
			fleet: domain.FleetConfig{
				EngineExtraEnv: map[string]string{
					"DAGGER_CACHE_TOKEN": "should-not-be-set",
				},
			},
			wantErr: true,
		},
		{
			name: "empty env name in engine_extra_env",
			fleet: domain.FleetConfig{
				EngineExtraEnv: map[string]string{
					"": "no-name",
				},
			},
			wantErr: true,
		},
		{
			name: "SSL_CERT_FILE in engine_extra_env with CA secret set",
			fleet: domain.FleetConfig{
				EngineCASecret: "custom-ca-bundle",
				EngineExtraEnv: map[string]string{
					"SSL_CERT_FILE": "/etc/ssl/certs/other.pem",
				},
			},
			wantErr: true,
		},
		{
			name: "SSL_CERT_FILE in engine_extra_env without CA secret",
			fleet: domain.FleetConfig{
				EngineExtraEnv: map[string]string{
					"SSL_CERT_FILE": "/etc/ssl/certs/other.pem",
				},
			},
			wantErr: false,
		},
		{
			name: "EngineCASecret set with empty EngineCASecretKey",
			fleet: domain.FleetConfig{
				EngineCASecret:    "custom-ca-bundle",
				EngineCASecretKey: "",
			},
			wantErr: true,
		},
		{
			name: "valid engine_extra_env_from entry",
			fleet: domain.FleetConfig{
				EngineExtraEnvFrom: map[string]domain.EnvVarSource{
					"HTTP_PROXY": {SecretName: "proxy-credentials", Key: "http_proxy"},
				},
			},
			wantErr: false,
		},
		{
			name: "reserved name in engine_extra_env_from",
			fleet: domain.FleetConfig{
				EngineExtraEnvFrom: map[string]domain.EnvVarSource{
					"DAGGER_CACHE_TOKEN": {SecretName: "s", Key: "k"},
				},
			},
			wantErr: true,
		},
		{
			name: "same name in both engine_extra_env and engine_extra_env_from",
			fleet: domain.FleetConfig{
				EngineExtraEnv: map[string]string{
					"HTTP_PROXY": "http://proxy:3128",
				},
				EngineExtraEnvFrom: map[string]domain.EnvVarSource{
					"HTTP_PROXY": {SecretName: "proxy-credentials", Key: "http_proxy"},
				},
			},
			wantErr: true,
		},
		{
			name: "engine_extra_env_from with empty SecretName",
			fleet: domain.FleetConfig{
				EngineExtraEnvFrom: map[string]domain.EnvVarSource{
					"HTTP_PROXY": {SecretName: "", Key: "http_proxy"},
				},
			},
			wantErr: true,
		},
		{
			name: "engine_extra_env_from with empty Key",
			fleet: domain.FleetConfig{
				EngineExtraEnvFrom: map[string]domain.EnvVarSource{
					"HTTP_PROXY": {SecretName: "proxy-credentials", Key: ""},
				},
			},
			wantErr: true,
		},
		{
			name: "empty name in engine_extra_env_from",
			fleet: domain.FleetConfig{
				EngineExtraEnvFrom: map[string]domain.EnvVarSource{
					"": {SecretName: "s", Key: "k"},
				},
			},
			wantErr: true,
		},
		{
			name: "SSL_CERT_FILE in engine_extra_env_from with CA secret set",
			fleet: domain.FleetConfig{
				EngineCASecret: "custom-ca-bundle",
				EngineExtraEnvFrom: map[string]domain.EnvVarSource{
					"SSL_CERT_FILE": {SecretName: "s", Key: "k"},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFleetEnv(&tt.fleet)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateFleetEnv() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
