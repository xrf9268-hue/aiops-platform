package workflow

import (
	"fmt"
	"os"
	"strings"
)

type MissingEnvValueError struct {
	Field  string
	EnvVar string
}

func (e *MissingEnvValueError) Error() string {
	category := "workflow_config_missing_value"
	if e.Field == "tracker.api_key" {
		category = "missing_tracker_api_key"
	}
	return fmt.Sprintf("%s: %s references $%s but the environment variable is unset or empty", category, e.Field, e.EnvVar)
}

func resolveExplicitEnv(field, value string) (string, error) {
	envName, ok := explicitEnvReferenceName(value)
	if !ok {
		return value, nil
	}
	resolved, ok := os.LookupEnv(envName)
	if !ok || resolved == "" {
		return "", &MissingEnvValueError{Field: field, EnvVar: envName}
	}
	return resolved, nil
}

func explicitEnvReferenceName(value string) (string, bool) {
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		name := strings.TrimSuffix(strings.TrimPrefix(value, "${"), "}")
		return name, isExplicitEnvName(name)
	}
	if strings.HasPrefix(value, "$") {
		name := strings.TrimPrefix(value, "$")
		return name, isExplicitEnvName(name)
	}
	return "", false
}

func isExplicitEnvName(name string) bool { //nolint:gocognit // baseline (#521)
	if name == "" {
		return false
	}
	isLetter := func(r rune) bool {
		return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
	}
	for i, r := range name {
		switch {
		case i == 0 && (r == '_' || isLetter(r)):
			continue
		case i > 0 && (r == '_' || isLetter(r) || (r >= '0' && r <= '9')):
			continue
		default:
			return false
		}
	}
	return true
}
