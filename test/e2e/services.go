//go:build e2e

package e2e

import (
	"context"
	"os"
)

type testbed struct {
	gitea  *giteaEnv
	cancel context.CancelFunc
}

func setupTestbed(ctx context.Context) (*testbed, error) {
	g, err := startGitea(ctx)
	if err != nil {
		return nil, err
	}

	_, cancel := context.WithCancel(ctx)
	return &testbed{
		gitea:  g,
		cancel: cancel,
	}, nil
}

func (b *testbed) close(ctx context.Context) {
	b.cancel()
	b.gitea.close(ctx)
}

func tmpDir() string {
	d, err := os.MkdirTemp("", "aiops-e2e-*")
	if err != nil {
		panic(err)
	}
	return d
}
