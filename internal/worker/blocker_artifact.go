package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	BlockerArtifactPath         = ".aiops/BLOCKED.json"
	blockerArtifactVersion      = 1
	blockerArtifactKindExternal = "external_dependency"
	DefaultBlockerRetryAfter    = time.Hour
	minBlockerRetryAfterSeconds = 60
	maxBlockerRetryAfterSeconds = 24 * 60 * 60
)

var ErrBlockerArtifactMissing = errors.New("blocked artifact missing")

type BlockerArtifact struct {
	Version           int    `json:"version"`
	Kind              string `json:"kind"`
	Reason            string `json:"reason"`
	RetryAfterSeconds int    `json:"retry_after_seconds"`
}

type ExternalBlockerError struct {
	Artifact BlockerArtifact
}

func (e *ExternalBlockerError) Error() string {
	reason := strings.TrimSpace(e.Artifact.Reason)
	if reason == "" {
		return "external dependency blocked run"
	}
	return "external dependency blocked run: " + reason
}

func (e *ExternalBlockerError) RetryAfter() time.Duration {
	if e == nil {
		return DefaultBlockerRetryAfter
	}
	return time.Duration(e.Artifact.RetryAfterSeconds) * time.Second
}

func ReadBlockerArtifact(workdir string) (BlockerArtifact, error) {
	path := filepath.Join(workdir, BlockerArtifactPath)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return BlockerArtifact{}, ErrBlockerArtifactMissing
	}
	if err != nil {
		return BlockerArtifact{}, fmt.Errorf("read %s: %w", BlockerArtifactPath, err)
	}
	defer func() { _ = f.Close() }()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var artifact BlockerArtifact
	if err := dec.Decode(&artifact); err != nil {
		return BlockerArtifact{}, fmt.Errorf("parse %s: %w", BlockerArtifactPath, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err != nil {
			return BlockerArtifact{}, fmt.Errorf("parse %s trailing content: %w", BlockerArtifactPath, err)
		}
		return BlockerArtifact{}, fmt.Errorf("parse %s: trailing JSON content", BlockerArtifactPath)
	}
	if err := artifact.validate(); err != nil {
		return BlockerArtifact{}, err
	}
	return artifact, nil
}

func ConsumeBlockerArtifact(workdir string) (BlockerArtifact, error) {
	artifact, err := ReadBlockerArtifact(workdir)
	if err != nil {
		return BlockerArtifact{}, err
	}
	path := filepath.Join(workdir, BlockerArtifactPath)
	if err := os.Remove(path); err != nil {
		return BlockerArtifact{}, fmt.Errorf("remove consumed %s: %w", BlockerArtifactPath, err)
	}
	return artifact, nil
}

func ResetBlockerArtifact(workdir string) error {
	path := filepath.Join(workdir, BlockerArtifactPath)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reset %s: %w", BlockerArtifactPath, err)
	}
	return nil
}

func (a BlockerArtifact) validate() error {
	if a.Version != blockerArtifactVersion {
		return fmt.Errorf("%s version = %d; want %d", BlockerArtifactPath, a.Version, blockerArtifactVersion)
	}
	if a.Kind != blockerArtifactKindExternal {
		return fmt.Errorf("%s kind = %q; want %q", BlockerArtifactPath, a.Kind, blockerArtifactKindExternal)
	}
	if strings.TrimSpace(a.Reason) == "" {
		return fmt.Errorf("%s reason is empty", BlockerArtifactPath)
	}
	if a.RetryAfterSeconds < minBlockerRetryAfterSeconds || a.RetryAfterSeconds > maxBlockerRetryAfterSeconds {
		return fmt.Errorf("%s retry_after_seconds = %d; want %d..%d", BlockerArtifactPath, a.RetryAfterSeconds, minBlockerRetryAfterSeconds, maxBlockerRetryAfterSeconds)
	}
	return nil
}
