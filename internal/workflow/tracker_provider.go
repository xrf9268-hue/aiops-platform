package workflow

import (
	"fmt"
	"os"

	"github.com/xrf9268-hue/aiops-platform/internal/trackerprofiles"
)

func admitTrackerProvider(path string, cfg *Config) error {
	profile, err := trackerprofiles.Resolve(
		cfg.Tracker.Kind,
		cfg.Tracker.Provider,
		cfg.Repo.Owner,
		cfg.Repo.Name,
		os.LookupEnv,
	)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	cfg.Tracker.Provider = profile.Provider()
	cfg.Tracker.profile = profile
	return nil
}
