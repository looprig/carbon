package app

import (
	"fmt"
	"os"
	"path/filepath"
)

// looprigHome resolves the looprig base directory: Config.HomeDir when set
// (must be absolute; fail closed otherwise), else ~/.looprig.
func looprigHome(cfg Config) (string, error) {
	if cfg.HomeDir != "" {
		if !filepath.IsAbs(cfg.HomeDir) {
			return "", fmt.Errorf("coderig: HomeDir must be absolute, got %q", cfg.HomeDir)
		}
		return cfg.HomeDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("coderig: resolve home directory: %w", err)
	}
	return filepath.Join(home, ".looprig"), nil
}

// LooprigHome is looprigHome's exported form. It exists solely for cmd/coderig
// (a separate package, so it cannot call the unexported resolver directly) to
// resolve the on-disk store root from the Config it builds via
// DefaultDataDirIn, without duplicating home-resolution logic. Every
// in-package caller keeps using looprigHome directly.
func LooprigHome(cfg Config) (string, error) {
	return looprigHome(cfg)
}
