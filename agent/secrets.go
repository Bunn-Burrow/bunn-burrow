package agent

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	// envNameRegex enforces POSIX compliant environment variable names: [a-zA-Z_][a-zA-Z0-9_]*
	envNameRegex = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

	// Sensitive key substring markers.
	sensitiveSubstrings = []string{
		"KEY",
		"SECRET",
		"TOKEN",
		"PASSWORD",
		"AUTH",
		"CREDENTIAL",
		"PRIVATE",
		"BEARER",
		"APIKEY",
	}

	// Well-known API key and token regex patterns to detect and mask in logs/output.
	knownSecretPatterns = []*regexp.Regexp{
		regexp.MustCompile(`sk-[a-zA-Z0-9_\-]{20,}`),         // OpenAI / Anthropic-style keys
		regexp.MustCompile(`gh[pousr]_[A-Za-z0-9_]{36,}`),    // GitHub tokens
		regexp.MustCompile(`[0-9a-fA-F]{32,64}`),             // Generic 32-64 hex tokens (if identified near auth context)
		regexp.MustCompile(`Bearer\s+[a-zA-Z0-9\-._~+/]+=*`), // HTTP Authorization Bearer tokens
	}
)

const RedactedPlaceholder = "[REDACTED]"

// ValidateEnvName checks if an environment variable name is valid according to POSIX rules.
func ValidateEnvName(name string) error {
	if name == "" {
		return fmt.Errorf("environment variable name cannot be empty")
	}
	if !envNameRegex.MatchString(name) {
		return fmt.Errorf("invalid environment variable name %q: must match ^[a-zA-Z_][a-zA-Z0-9_]*$", name)
	}
	return nil
}

// IsSensitiveKey reports whether an environment variable name likely represents a secret.
func IsSensitiveKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, marker := range sensitiveSubstrings {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

// SanitizeEnv produces a sanitized copy of env vars where any secret key or sensitive key
// is replaced with RedactedPlaceholder.
func SanitizeEnv(env map[string]string, secrets map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		if IsSensitiveKey(k) {
			out[k] = RedactedPlaceholder
		} else {
			out[k] = v
		}
	}
	// For any key in secrets, ensure it is either redacted or not leaked
	for k := range secrets {
		out[k] = RedactedPlaceholder
	}
	return out
}

// PrepareExecutionEnv merges non-sensitive environment variables with secrets into
// the "KEY=VALUE" slice expected by the guest execution layer.
// Validates all variable names. Secrets are kept only in memory for the execution request.
func PrepareExecutionEnv(env map[string]string, secrets map[string]string) ([]string, error) {
	result := make([]string, 0, len(env)+len(secrets))

	// Merge regular env
	for k, v := range env {
		if err := ValidateEnvName(k); err != nil {
			return nil, err
		}
		result = append(result, fmt.Sprintf("%s=%s", k, v))
	}

	// Merge secrets (overriding duplicate non-secret keys if any)
	for k, v := range secrets {
		if err := ValidateEnvName(k); err != nil {
			return nil, err
		}
		result = append(result, fmt.Sprintf("%s=%s", k, v))
	}

	return result, nil
}

// RedactText scrubs secret literal values and known token patterns from output text,
// ensuring logs and console output remain safe.
func RedactText(text string, secrets map[string]string) string {
	if text == "" {
		return text
	}

	res := text

	// 1. Redact explicit secret values (if length >= 4 to avoid over-redacting short common strings)
	for _, secretVal := range secrets {
		if len(secretVal) >= 4 {
			res = strings.ReplaceAll(res, secretVal, RedactedPlaceholder)
		}
	}

	// 2. Redact known token patterns
	for _, pat := range knownSecretPatterns {
		res = pat.ReplaceAllStringFunc(res, func(matched string) string {
			if strings.HasPrefix(matched, "Bearer ") {
				return "Bearer " + RedactedPlaceholder
			}
			return RedactedPlaceholder
		})
	}

	return res
}
