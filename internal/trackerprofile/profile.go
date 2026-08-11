package trackerprofile

import (
	"fmt"
	"strings"
)

// Profile is the adapter-owned interpretation of tracker.provider. Core
// workflow code uses only its secret metadata and opaque provider projection.
type Profile interface {
	Kind() string
	Provider() map[string]any
	SecretKeys() []string
	SecretEnvironmentNames() []string
	SecretValues() []string
}

// Base carries the provider data shared by adapter-specific profiles.
type Base struct {
	AdapterKind  string
	ProviderMap  map[string]any
	Secrets      []string
	SecretEnvs   []string
	SecretFields []string
}

func (b Base) Kind() string                     { return b.AdapterKind }
func (b Base) Provider() map[string]any         { return CloneProvider(b.ProviderMap) }
func (b Base) SecretKeys() []string             { return append([]string(nil), b.SecretFields...) }
func (b Base) SecretEnvironmentNames() []string { return append([]string(nil), b.SecretEnvs...) }
func (b Base) SecretValues() []string           { return append([]string(nil), b.Secrets...) }

// CloneProvider copies nested YAML maps and slices so masking a display copy
// cannot mutate the admitted runtime configuration.
func CloneProvider(provider map[string]any) map[string]any {
	if provider == nil {
		return map[string]any{}
	}
	cloned := make(map[string]any, len(provider))
	for key, value := range provider {
		cloned[key] = cloneValue(value)
	}
	return cloned
}

func cloneValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return CloneProvider(value)
	case []any:
		cloned := make([]any, len(value))
		for i := range value {
			cloned[i] = cloneValue(value[i])
		}
		return cloned
	default:
		return value
	}
}

// String reads one adapter-documented string without interpreting unknown
// provider keys.
func String(provider map[string]any, key string) (string, bool, error) {
	value, present := provider[key]
	if !present {
		return "", false, nil
	}
	text, ok := value.(string)
	if !ok {
		return "", true, fmt.Errorf("tracker.provider.%s must be a string", key)
	}
	return text, true, nil
}

// PaginationPages reads an optional non-negative page cap. Zero retains the
// selected adapter's default, matching the pre-cutover pagination contract.
func PaginationPages(provider map[string]any, key string, fallback int) (int, error) {
	value, present := provider[key]
	if !present {
		return fallback, nil
	}
	var number int
	switch value := value.(type) {
	case int:
		number = value
	case int64:
		number = int(value)
	case uint64:
		number = int(value)
	case float64:
		if value != float64(int(value)) {
			return 0, fmt.Errorf("tracker.provider.%s must be zero for the adapter default or a positive integer", key)
		}
		number = int(value)
	default:
		return 0, fmt.Errorf("tracker.provider.%s must be zero for the adapter default or a positive integer", key)
	}
	if number < 0 {
		return 0, fmt.Errorf("tracker.provider.%s must be zero for the adapter default or a positive integer", key)
	}
	if number == 0 {
		return fallback, nil
	}
	return number, nil
}

// ResolveDocumentedString expands an exact $VAR or ${VAR} reference only for
// a key declared by the selected adapter.
func ResolveDocumentedString(provider map[string]any, key string, lookup func(string) (string, bool)) (string, string, bool, error) {
	value, present, err := String(provider, key)
	if err != nil || !present {
		return value, "", present, err
	}
	envName, isReference := explicitEnvReference(value)
	if !isReference {
		return value, "", true, nil
	}
	resolved, ok := lookup(envName)
	if !ok {
		return "", envName, true, fmt.Errorf("tracker.provider.%s references $%s, but the environment variable is missing", key, envName)
	}
	provider[key] = resolved
	return resolved, envName, true, nil
}

// ResolveRequiredSecret resolves a documented secret and treats an empty
// literal or resolved value as missing without putting that value in the error.
func ResolveRequiredSecret(provider map[string]any, key string, aliases []string, lookup func(string) (string, bool)) (string, []string, error) {
	value, envs, present, err := ResolveOptionalSecret(provider, key, aliases, lookup)
	if err != nil {
		return "", envs, err
	}
	if !present {
		return "", envs, fmt.Errorf("missing_tracker_secret: tracker.provider.%s is required", key)
	}
	return value, envs, nil
}

// ResolveOptionalSecret resolves a present documented secret while allowing
// an absent key so adapter runtime constructors can preserve typed legacy
// missing-credential errors. An explicitly empty value still fails admission.
func ResolveOptionalSecret(provider map[string]any, key string, aliases []string, lookup func(string) (string, bool)) (string, []string, bool, error) {
	value, envName, present, err := ResolveDocumentedString(provider, key, lookup)
	envs := uniqueNames(aliases, envName)
	if err != nil {
		return "", envs, present, fmt.Errorf("missing_tracker_secret: %w", err)
	}
	if !present {
		return "", envs, false, nil
	}
	if strings.TrimSpace(value) == "" {
		if envName != "" {
			return "", envs, true, fmt.Errorf("missing_tracker_secret: tracker.provider.%s references $%s, but it resolved to an empty value", key, envName)
		}
		return "", envs, true, fmt.Errorf("missing_tracker_secret: tracker.provider.%s is required", key)
	}
	return value, envs, true, nil
}

func explicitEnvReference(value string) (string, bool) {
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		name := value[2 : len(value)-1]
		return name, validEnvironmentName(name)
	}
	if strings.HasPrefix(value, "$") && len(value) > 1 {
		name := value[1:]
		return name, validEnvironmentName(name)
	}
	return "", false
}

func validEnvironmentName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if !validEnvironmentCharacter(r, i == 0) {
			return false
		}
	}
	return true
}

func validEnvironmentCharacter(r rune, first bool) bool {
	if r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' {
		return true
	}
	return !first && r >= '0' && r <= '9'
}

// EnvironmentValue returns a non-empty environment value or fallback.
func EnvironmentValue(lookup func(string) (string, bool), name, fallback string) string {
	if value, ok := lookup(name); ok && strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func uniqueNames(base []string, extra string) []string {
	result := make([]string, 0, len(base)+1)
	seen := make(map[string]struct{}, len(base)+1)
	for _, name := range append(append([]string(nil), base...), extra) {
		if name == "" {
			continue
		}
		normalized := strings.ToUpper(name)
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		result = append(result, name)
	}
	return result
}
