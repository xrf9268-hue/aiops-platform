package workflow

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	CodexSandboxDangerFullAccess = "dangerFullAccess"
	CodexSandboxReadOnly         = "readOnly"
	CodexSandboxExternalSandbox  = "externalSandbox"
	CodexSandboxWorkspaceWrite   = "workspaceWrite"
)

type CodexSandboxPolicy struct {
	Type                  string
	WritableRoots         []string
	NetworkAccess         bool
	ExternalNetworkAccess string
	ExcludeTmpdirEnvVar   bool
	ExcludeSlashTmp       bool
}

func DefaultCodexSandboxPolicy() CodexSandboxPolicy {
	return CodexSandboxPolicy{
		Type:                CodexSandboxWorkspaceWrite,
		WritableRoots:       []string{},
		NetworkAccess:       false,
		ExcludeTmpdirEnvVar: false,
		ExcludeSlashTmp:     false,
	}
}

func (p CodexSandboxPolicy) IsZero() bool {
	return p.Type == "" &&
		len(p.WritableRoots) == 0 &&
		!p.NetworkAccess &&
		p.ExternalNetworkAccess == "" &&
		!p.ExcludeTmpdirEnvVar &&
		!p.ExcludeSlashTmp
}

func (p *CodexSandboxPolicy) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Tag == "!!null" {
		*p = CodexSandboxPolicy{}
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("codex.turn_sandbox_policy must be a map with a type field")
	}

	fields := map[string]*yaml.Node{}
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if _, ok := fields[key]; ok {
			return fmt.Errorf("codex.turn_sandbox_policy.%s is duplicated", key)
		}
		fields[key] = node.Content[i+1]
	}
	for _, legacy := range []string{"mode", "readOnlyAccess", "access"} {
		if _, ok := fields[legacy]; ok {
			return fmt.Errorf("codex.turn_sandbox_policy.%s is not supported; use the current Codex app-server typed sandboxPolicy schema", legacy)
		}
	}

	typeNode, ok := fields["type"]
	if !ok || strings.TrimSpace(typeNode.Value) == "" {
		return fmt.Errorf("codex.turn_sandbox_policy.type is required")
	}
	policy := CodexSandboxPolicy{Type: typeNode.Value}
	if err := rejectUnknownCodexSandboxFields(policy.Type, fields); err != nil {
		return err
	}

	switch policy.Type {
	case CodexSandboxDangerFullAccess:
	case CodexSandboxReadOnly:
		if err := requireBoolCodexSandboxField(fields, "networkAccess", &policy.NetworkAccess); err != nil {
			return err
		}
	case CodexSandboxExternalSandbox:
		network, err := requireStringCodexSandboxField(fields, "networkAccess")
		if err != nil {
			return err
		}
		if network != "restricted" && network != "enabled" {
			return fmt.Errorf("codex.turn_sandbox_policy.networkAccess = %q, want restricted or enabled", network)
		}
		policy.ExternalNetworkAccess = network
	case CodexSandboxWorkspaceWrite:
		if err := requireStringSliceCodexSandboxField(fields, "writableRoots", &policy.WritableRoots); err != nil {
			return err
		}
		if err := requireBoolCodexSandboxField(fields, "networkAccess", &policy.NetworkAccess); err != nil {
			return err
		}
		if err := requireBoolCodexSandboxField(fields, "excludeTmpdirEnvVar", &policy.ExcludeTmpdirEnvVar); err != nil {
			return err
		}
		if err := requireBoolCodexSandboxField(fields, "excludeSlashTmp", &policy.ExcludeSlashTmp); err != nil {
			return err
		}
	default:
		return fmt.Errorf("codex.turn_sandbox_policy.type %q is not supported (allowed: dangerFullAccess, readOnly, externalSandbox, workspaceWrite)", policy.Type)
	}
	*p = policy
	return nil
}

func (p CodexSandboxPolicy) MarshalJSON() ([]byte, error) {
	switch p.Type {
	case "":
		return []byte("null"), nil
	case CodexSandboxDangerFullAccess:
		return json.Marshal(struct {
			Type string `json:"type"`
		}{Type: p.Type})
	case CodexSandboxReadOnly:
		return json.Marshal(struct {
			Type          string `json:"type"`
			NetworkAccess bool   `json:"networkAccess"`
		}{Type: p.Type, NetworkAccess: p.NetworkAccess})
	case CodexSandboxExternalSandbox:
		return json.Marshal(struct {
			Type          string `json:"type"`
			NetworkAccess string `json:"networkAccess"`
		}{Type: p.Type, NetworkAccess: p.ExternalNetworkAccess})
	case CodexSandboxWorkspaceWrite:
		roots := p.WritableRoots
		if roots == nil {
			roots = []string{}
		}
		return json.Marshal(struct {
			Type                string   `json:"type"`
			WritableRoots       []string `json:"writableRoots"`
			NetworkAccess       bool     `json:"networkAccess"`
			ExcludeTmpdirEnvVar bool     `json:"excludeTmpdirEnvVar"`
			ExcludeSlashTmp     bool     `json:"excludeSlashTmp"`
		}{
			Type:                p.Type,
			WritableRoots:       roots,
			NetworkAccess:       p.NetworkAccess,
			ExcludeTmpdirEnvVar: p.ExcludeTmpdirEnvVar,
			ExcludeSlashTmp:     p.ExcludeSlashTmp,
		})
	default:
		return nil, fmt.Errorf("unsupported Codex sandbox policy type %q", p.Type)
	}
}

func (p CodexSandboxPolicy) Validate(field string) error {
	if p.IsZero() {
		return fmt.Errorf("%s.type is required", field)
	}
	switch p.Type {
	case CodexSandboxDangerFullAccess, CodexSandboxReadOnly, CodexSandboxWorkspaceWrite:
		return nil
	case CodexSandboxExternalSandbox:
		if p.ExternalNetworkAccess != "restricted" && p.ExternalNetworkAccess != "enabled" {
			return fmt.Errorf("%s.networkAccess = %q, want restricted or enabled", field, p.ExternalNetworkAccess)
		}
		return nil
	default:
		return fmt.Errorf("%s.type %q is not supported", field, p.Type)
	}
}

func rejectUnknownCodexSandboxFields(policyType string, fields map[string]*yaml.Node) error {
	allowed := map[string]bool{"type": true}
	switch policyType {
	case CodexSandboxDangerFullAccess:
	case CodexSandboxReadOnly, CodexSandboxExternalSandbox:
		allowed["networkAccess"] = true
	case CodexSandboxWorkspaceWrite:
		allowed["writableRoots"] = true
		allowed["networkAccess"] = true
		allowed["excludeTmpdirEnvVar"] = true
		allowed["excludeSlashTmp"] = true
	default:
		return nil
	}
	var unknown []string
	for key := range fields {
		if !allowed[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("codex.turn_sandbox_policy has unsupported field(s): %s", strings.Join(unknown, ", "))
}

func requireBoolCodexSandboxField(fields map[string]*yaml.Node, key string, out *bool) error {
	node, ok := fields[key]
	if !ok {
		return fmt.Errorf("codex.turn_sandbox_policy.%s is required", key)
	}
	if err := node.Decode(out); err != nil {
		return fmt.Errorf("codex.turn_sandbox_policy.%s must be a boolean: %w", key, err)
	}
	return nil
}

func requireStringCodexSandboxField(fields map[string]*yaml.Node, key string) (string, error) {
	node, ok := fields[key]
	if !ok {
		return "", fmt.Errorf("codex.turn_sandbox_policy.%s is required", key)
	}
	var out string
	if err := node.Decode(&out); err != nil {
		return "", fmt.Errorf("codex.turn_sandbox_policy.%s must be a string: %w", key, err)
	}
	return out, nil
}

func requireStringSliceCodexSandboxField(fields map[string]*yaml.Node, key string, out *[]string) error {
	node, ok := fields[key]
	if !ok {
		return fmt.Errorf("codex.turn_sandbox_policy.%s is required", key)
	}
	if err := node.Decode(out); err != nil {
		return fmt.Errorf("codex.turn_sandbox_policy.%s must be a string array: %w", key, err)
	}
	if *out == nil {
		*out = []string{}
	}
	return nil
}
