package app

import (
	"fmt"
	"os"
	"path/filepath"
)

// looprigHome resolves Carbon's base directory: Config.HomeDir when set (must be
// absolute, used exactly as given; fail closed otherwise), else ~/.looprig/carbon.
// This directory is Carbon-specific, not shared with any other looprig-platform agent
// product: harness's sessionstore/workspacestore have no notion of "which product" is
// calling them, so namespacing by product is entirely this resolver's job. It remains ONE
// file/store per machine per product (not per project or invocation) — models.json's inline
// API keys and the session store both still live once here, just scoped to Carbon rather
// than shared across every possible looprig-based agent.
func looprigHome(cfg Config) (string, error) {
	if cfg.HomeDir != "" {
		if !filepath.IsAbs(cfg.HomeDir) {
			return "", fmt.Errorf("carbon: HomeDir must be absolute, got %q", cfg.HomeDir)
		}
		return cfg.HomeDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("carbon: resolve home directory: %w", err)
	}
	return filepath.Join(home, ".looprig", "carbon"), nil
}

// LooprigHome is looprigHome's exported form. It exists solely for cmd/carbon
// (a separate package, so it cannot call the unexported resolver directly) to
// resolve the on-disk store root from the Config it builds via
// DefaultDataDirIn, without duplicating home-resolution logic. Every
// in-package caller keeps using looprigHome directly.
func LooprigHome(cfg Config) (string, error) {
	return looprigHome(cfg)
}
