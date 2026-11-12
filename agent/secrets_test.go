package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateEnvName(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"valid lowercase", "api_key", false},
		{"valid uppercase", "OPENAI_API_KEY", false},
		{"valid mixed", "MyVar_123", false},
		{"starts with underscore", "_HIDDEN", false},
		{"empty string", "", true},
		{"starts with number", "1KEY", true},
		{"contains dash", "API-KEY", true},
		{"contains special char", "KEY$NAME", true},
		{"contains space", "API KEY", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateEnvName(tt.key)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestIsSensitiveKey(t *testing.T) {
	require.True(t, IsSensitiveKey("OPENAI_API_KEY"))
	require.True(t, IsSensitiveKey("ANTHROPIC_AUTH_TOKEN"))
	require.True(t, IsSensitiveKey("DB_PASSWORD"))
	require.True(t, IsSensitiveKey("MY_SECRET"))
	require.True(t, IsSensitiveKey("USER_CREDENTIALS"))
	require.True(t, IsSensitiveKey("PRIVATE_KEY"))
	require.True(t, IsSensitiveKey("BEARER_TOKEN"))
	require.False(t, IsSensitiveKey("PORT"))
	require.False(t, IsSensitiveKey("ENV"))
	require.False(t, IsSensitiveKey("DEBUG"))
	require.False(t, IsSensitiveKey("WORKSPACE_DIR"))
}

func TestSanitizeEnv(t *testing.T) {
	env := map[string]string{
		"DEBUG":          "true",
		"PORT":           "8080",
		"OPENAI_API_KEY": "sk-1234567890abcdef1234567890",
	}
	secrets := map[string]string{
		"CUSTOM_TOKEN": "secret-val-xyz",
	}

	sanitized := SanitizeEnv(env, secrets)
	require.Equal(t, "true", sanitized["DEBUG"])
	require.Equal(t, "8080", sanitized["PORT"])
	require.Equal(t, RedactedPlaceholder, sanitized["OPENAI_API_KEY"])
	require.Equal(t, RedactedPlaceholder, sanitized["CUSTOM_TOKEN"])
}

func TestPrepareExecutionEnv(t *testing.T) {
	env := map[string]string{
		"MODE": "production",
	}
	secrets := map[string]string{
		"API_KEY": "secret-123",
	}

	result, err := PrepareExecutionEnv(env, secrets)
	require.NoError(t, err)
	require.Contains(t, result, "MODE=production")
	require.Contains(t, result, "API_KEY=secret-123")

	// Test invalid env key
	_, err = PrepareExecutionEnv(map[string]string{"INVALID-KEY": "val"}, nil)
	require.Error(t, err)

	// Test invalid secret key
	_, err = PrepareExecutionEnv(nil, map[string]string{"1INVALID": "val"})
	require.Error(t, err)
}

func TestRedactText(t *testing.T) {
	secrets := map[string]string{
		"API_KEY": "supersecretkey999",
	}

	raw := "Agent started with key supersecretkey999 and token sk-abcdef1234567890abcdef123456 and Bearer eyJhbGciOiJIUzI1NiJ9"
	scrubbed := RedactText(raw, secrets)

	require.NotContains(t, scrubbed, "supersecretkey999")
	require.NotContains(t, scrubbed, "sk-abcdef1234567890abcdef123456")
	require.NotContains(t, scrubbed, "Bearer eyJhbGci")
	require.Contains(t, scrubbed, RedactedPlaceholder)
}
