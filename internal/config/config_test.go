package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoad_RequiresAPIKeys(t *testing.T) {
	t.Setenv(envAPIKeys, "")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when DOCPIPE_API_KEYS is unset")
	} else if !strings.Contains(err.Error(), envAPIKeys) {
		t.Fatalf("error should mention env var, got: %v", err)
	}
}

func TestLoad_Defaults(t *testing.T) {
	t.Setenv(envAPIKeys, "default:secret123")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Port != 8080 {
		t.Errorf("port default: got %d, want 8080", c.Port)
	}
	if c.RenderTimeout != 30*time.Second {
		t.Errorf("render timeout default: got %v, want 30s", c.RenderTimeout)
	}
	if c.MaxBodyBytes != 10*1024*1024 {
		t.Errorf("max body default: got %d, want 10MiB", c.MaxBodyBytes)
	}
	if !c.StatsPublic {
		t.Error("stats should be public by default")
	}
	if c.EnableSwagger {
		t.Error("swagger should be disabled in production by default")
	}
}

func TestLoad_DevelopmentEnablesSwagger(t *testing.T) {
	t.Setenv(envAPIKeys, "dev:k")
	t.Setenv(envEnv, EnvDevelopment)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.EnableSwagger {
		t.Error("DOCPIPE_ENV=development should enable swagger by default")
	}
}

func TestLoad_AccumulatesErrors(t *testing.T) {
	t.Setenv(envAPIKeys, "ok:secret")
	t.Setenv(envPort, "70000")
	t.Setenv(envLogLevel, "shout")
	t.Setenv(envRenderTimeout, "not-a-duration")
	_, err := Load()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, want := range []string{envPort, envLogLevel, envRenderTimeout} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s: %v", want, err)
		}
	}
}

func TestParseAPIKeys(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    map[string]string
		wantErr string
	}{
		{"single", "alpha:secret", map[string]string{"alpha": "secret"}, ""},
		{"multiple with spaces", " alpha:s1 , beta:s2 ", map[string]string{"alpha": "s1", "beta": "s2"}, ""},
		{"empty", "", nil, "required"},
		{"missing colon", "alpha", nil, "malformed"},
		{"empty secret", "alpha:", nil, "malformed"},
		{"empty name", ":secret", nil, "malformed"},
		{"duplicate name", "alpha:s1,alpha:s2", nil, "duplicate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAPIKeys(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len: got %d want %d", len(got), len(tc.want))
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("key %q: got %q want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestParseList(t *testing.T) {
	got := parseList(" a, b ,, c ,")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("len: got %d want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q want %q", i, got[i], want[i])
		}
	}
}
