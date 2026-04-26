package config

import (
	"strings"
	"testing"
)

// E5: Load returns errors for malformed env, it does not panic.
func TestLoad_MalformedIntReturnsError(t *testing.T) {
	t.Setenv("API_KEY", "k")
	t.Setenv("WORKER_CONCURRENCY", "abc")
	_, err := Load()
	if err == nil {
		t.Fatal("expected an error for non-integer WORKER_CONCURRENCY")
	}
	if !strings.Contains(err.Error(), "WORKER_CONCURRENCY") {
		t.Errorf("error should name the offending env var, got: %v", err)
	}
}

func TestLoad_MissingAPIKey(t *testing.T) {
	t.Setenv("API_KEY", "")
	_, err := Load()
	if err == nil {
		t.Fatal("missing API_KEY must be rejected")
	}
}

// M3: CORS_ALLOW_ORIGINS parsing.
func TestLoad_CORSAllowOrigins(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want []string
	}{
		{"default star", "", []string{"*"}},
		{"explicit star", "*", []string{"*"}},
		{"single origin", "https://app.example.com", []string{"https://app.example.com"}},
		{"comma list with spaces", "https://a.example, https://b.example ,https://c.example",
			[]string{"https://a.example", "https://b.example", "https://c.example"}},
		{"empty entries dropped", "https://a.example,,  ,https://b.example",
			[]string{"https://a.example", "https://b.example"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("API_KEY", "k")
			if tc.env == "" {
				t.Setenv("CORS_ALLOW_ORIGINS", "")
			} else {
				t.Setenv("CORS_ALLOW_ORIGINS", tc.env)
			}
			c, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if !stringSliceEqual(c.CORSAllowOrigins, tc.want) {
				t.Errorf("got %v want %v", c.CORSAllowOrigins, tc.want)
			}
		})
	}
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("API_KEY", "k")
	// Ensure no integer envs are set (would otherwise inherit from host).
	t.Setenv("WORKER_CONCURRENCY", "")
	t.Setenv("RATE_LIMIT_PER_MINUTE", "")
	t.Setenv("SMTP_PORT", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.WorkerConcurrency != 20 || c.RateLimitPerMinute != 60 || c.SMTPPort != 1025 {
		t.Errorf("defaults not applied: %+v", c)
	}
}
