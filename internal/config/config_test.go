package config

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func validEnv() map[string]string {
	return map[string]string{
		"POSTGRES_HOST":                "pg.example",
		"POSTGRES_PASSWORD":            "pgsecret",
		"CLICKHOUSE_HOST":              "ch.example",
		"CLICKHOUSE_ADMIN_PASSWORD":    "chadmin",
		"CLICKHOUSE_READONLY_PASSWORD": "chro",
	}
}

func TestLoad_happyPathAppliesDefaults(t *testing.T) {
	setEnv(t, validEnv())
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Port != 8080 {
		t.Errorf("Port default: got %d want 8080", c.Port)
	}
	if c.PostgresPort != 5432 {
		t.Errorf("PostgresPort default: got %d want 5432", c.PostgresPort)
	}
	if c.PostgresDB != "mere" {
		t.Errorf("PostgresDB default: got %q want mere", c.PostgresDB)
	}
	if c.ClickHouseAdminUser != "mere_admin" {
		t.Errorf("ClickHouseAdminUser default: got %q want mere_admin", c.ClickHouseAdminUser)
	}
	if c.ClickHouseReadonlyUser != "mere_readonly" {
		t.Errorf("ClickHouseReadonlyUser default: got %q want mere_readonly", c.ClickHouseReadonlyUser)
	}
}

func TestLoad_missingRequiredVars(t *testing.T) {
	required := []string{
		"POSTGRES_HOST",
		"POSTGRES_PASSWORD",
		"CLICKHOUSE_HOST",
		"CLICKHOUSE_ADMIN_PASSWORD",
		"CLICKHOUSE_READONLY_PASSWORD",
	}
	for _, missing := range required {
		t.Run("missing_"+missing, func(t *testing.T) {
			env := validEnv()
			delete(env, missing)
			setEnv(t, env)
			if _, err := Load(); err == nil {
				t.Fatalf("Load with missing %s: expected error, got nil", missing)
			}
		})
	}
}

func TestLoad_rejectsExplicitEmptyString(t *testing.T) {
	env := validEnv()
	env["POSTGRES_HOST"] = ""
	setEnv(t, env)
	_, err := Load()
	if err == nil {
		t.Fatal("Load with empty POSTGRES_HOST: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "POSTGRES_HOST") {
		t.Errorf("error should mention POSTGRES_HOST: %v", err)
	}
}

func TestLoad_rejectsBadPort(t *testing.T) {
	env := validEnv()
	env["PORT"] = "0"
	setEnv(t, env)
	if _, err := Load(); err == nil {
		t.Fatal("PORT=0: expected error")
	}
}

// TestLogValue_redactsPasswords asserts that every password field's value
// is replaced with [REDACTED] in the logged output.
func TestLogValue_redactsPasswords(t *testing.T) {
	c := Config{
		Port:                       8080,
		PostgresHost:               "pg.example",
		PostgresPort:               5432,
		PostgresDB:                 "mere",
		PostgresUser:               "mere",
		PostgresPassword:           "PG-PLAINTEXT-LEAK",
		ClickHouseHost:             "ch.example",
		ClickHousePort:             9000,
		ClickHouseDatabase:         "analytics",
		ClickHouseAdminUser:        "mere_admin",
		ClickHouseAdminPassword:    "CH-ADMIN-PLAINTEXT-LEAK",
		ClickHouseReadonlyUser:     "mere_readonly",
		ClickHouseReadonlyPassword: "CH-RO-PLAINTEXT-LEAK",
	}
	out := logToJSON(t, c)
	for _, leaked := range []string{
		"PG-PLAINTEXT-LEAK",
		"CH-ADMIN-PLAINTEXT-LEAK",
		"CH-RO-PLAINTEXT-LEAK",
	} {
		if strings.Contains(out, leaked) {
			t.Errorf("LogValue leaked %q in output: %s", leaked, out)
		}
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("LogValue should contain [REDACTED] markers: %s", out)
	}
}

// TestLogValue_coversAllConfigFields uses reflection to walk Config's
// exported fields and assert each one's envconfig name appears in the
// logged output. Catches drift if a new field is added but forgotten
// in LogValue().
func TestLogValue_coversAllConfigFields(t *testing.T) {
	c := Config{}
	out := logToJSON(t, c)

	typ := reflect.TypeOf(c)
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		envName := field.Tag.Get("envconfig")
		if envName == "" {
			t.Errorf("field %s has no envconfig tag; LogValue check skipped", field.Name)
			continue
		}
		// LogValue uses lowercased envconfig names as keys.
		key := strings.ToLower(envName)
		if !strings.Contains(out, `"`+key+`"`) {
			t.Errorf("LogValue missing key for field %s (env %s, expected key %q) — add it to LogValue()", field.Name, envName, key)
		}
	}
}

func logToJSON(t *testing.T, c Config) string {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("cfg", "cfg", c)
	// Validate it's parseable JSON.
	var anyMap map[string]any
	if err := json.Unmarshal(buf.Bytes(), &anyMap); err != nil {
		t.Fatalf("logged output is not valid JSON: %v\n%s", err, buf.String())
	}
	return buf.String()
}
