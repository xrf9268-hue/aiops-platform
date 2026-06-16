package workflow

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Workflow struct {
	Path           string
	Config         Config
	PromptTemplate string
	Source         Source
}

func Load(path string) (*Workflow, error) { //nolint:gocognit // baseline (#521)
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &Error{Category: CategoryMissingWorkflowFile, Path: path, Message: "read workflow file", Err: err}
		}
		return nil, fmt.Errorf("%s: read workflow file: %w", path, err)
	}
	front, body := splitFrontMatter(string(b))
	cfg := DefaultConfig()
	hasFrontMatter := strings.TrimSpace(front) != ""
	if hasFrontMatter {
		frontBytes := []byte(front)
		if err := validateFrontMatterRoot(path, frontBytes); err != nil {
			return nil, err
		}
		if err := rejectRemovedFields(frontBytes); err != nil {
			return nil, err
		}
		logUnknownTopLevelKeys(frontBytes)
		hookFields := hookFieldPresence(frontBytes, "hooks")
		workspaceRootSet := hasNestedKey(frontBytes, "workspace", "root")
		serverPortSet := hasNestedKey(frontBytes, "server", "port")
		turnSandboxPolicySet := hasNestedKey(frontBytes, "codex", "turn_sandbox_policy")
		if err := yaml.Unmarshal(frontBytes, &cfg); err != nil {
			return nil, &Error{Category: CategoryWorkflowParseError, Path: path, Message: "parse workflow front matter", Err: err}
		}
		cfg.hookFields = hookFields
		cfg.Workspace.rootSet = workspaceRootSet
		cfg.Server.portSet = serverPortSet
		cfg.Codex.turnSandboxPolicySet = turnSandboxPolicySet
	}
	var rawStateCaps map[string]int
	if hasFrontMatter && len(cfg.Agent.MaxConcurrentAgentsByState) > 0 {
		rawStateCaps = make(map[string]int, len(cfg.Agent.MaxConcurrentAgentsByState))
		for state, limit := range cfg.Agent.MaxConcurrentAgentsByState {
			rawStateCaps[state] = limit
		}
	}
	defaultAgentMaxContinuationTurns(&cfg, hasFrontMatter, []byte(front))
	if err := expandConfigForWorkflowPath(path, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if rawStateCaps != nil {
		cfg.Agent.MaxConcurrentAgentsByState = rawStateCaps
	}
	// Only validate when the file actually carries a front-matter block.
	// Prompt-only WORKFLOW.md files are a supported pattern for repos that
	// rely on the worker's built-in defaults (same semantic as Resolve's
	// no-file fallback to Source=default). Forcing schema validation on those
	// would regress every repo that has not yet adopted the explicit
	// Symphony front matter.
	if hasFrontMatter {
		if err := validateConfig(path, cfg); err != nil {
			return nil, err
		}
	}
	normalizeLoadedConfig(&cfg)
	source := SourceFile
	if !hasFrontMatter {
		source = SourcePromptOnly
	}
	return &Workflow{Path: path, Config: cfg, PromptTemplate: strings.TrimSpace(body), Source: source}, nil
}

func validateFrontMatterRoot(path string, frontBytes []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(frontBytes, &doc); err != nil {
		return &Error{Category: CategoryWorkflowParseError, Path: path, Message: "parse workflow front matter", Err: err}
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return &Error{Category: CategoryWorkflowFrontMatterNotMap, Path: path, Message: "workflow front matter must decode to a map"}
	}
	return nil
}
