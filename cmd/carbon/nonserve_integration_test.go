//go:build integration

package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// R1.5 steps 1 and 6 at the process root. The non-browser paths read their own
// store without composing Factory or Host even when a complete browser
// configuration is supplied, and browser serve over that same store refuses the
// legacy-marked root without adopting it; the non-browser path still reads it
// afterwards. (The TUI shares --list's SessionStoreFactory and differs only in
// runtime.Run, which needs a terminal; its dispatch is covered by
// TestRunCLIClosesSessionBeforeStore.)
func TestNonBrowserPathReadsItsStoreWithoutFactoryAndServeRefusesIt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	dataDir := filepath.Join(t.TempDir(), "store")
	config := lifecycleBrowserConfig(dataDir)
	list := func(stage string) {
		t.Helper()
		var out, errOut syncBuffer
		// Bounded: a regression that routed --list into the browser lifecycle
		// would otherwise serve until the test binary's own timeout.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if code := runWithBrowserConfig(ctx, []string{"--list", "--data-dir", dataDir}, config, &out, &errOut); code != exitOK {
			t.Fatalf("%s: --list exit = %d stderr %q", stage, code, errOut.String())
		}
		if !strings.Contains(out.String(), "no sessions yet") {
			t.Fatalf("%s: --list output %q", stage, out.String())
		}
		for _, browserOnly := range []string{"tenant-journals", "session-workspaces"} {
			if _, err := os.Stat(filepath.Join(dataDir, browserOnly)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("%s: non-browser path created browser-only %s: %v", stage, browserOnly, err)
			}
		}
	}
	list("before serve")

	var out, errOut syncBuffer
	code := runWithBrowserConfig(context.Background(), []string{"serve", "--addr", "127.0.0.1:0", "--data-dir", dataDir,
		"--access-profile", "trusted"}, config, &out, &errOut)
	if code == exitOK || !strings.Contains(errOut.String(), "disagrees with persisted marker") {
		t.Fatalf("serve over the non-browser store = %d stderr %q, want a layout refusal", code, errOut.String())
	}
	if strings.Contains(out.String(), "http://") {
		t.Fatalf("serve over the non-browser store listened: %q", out.String())
	}
	list("after refused serve")
}
