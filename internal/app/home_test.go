package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLooprigHome(t *testing.T) {
	t.Run("empty uses user home", func(t *testing.T) {
		got, err := looprigHome(Config{})
		if err != nil {
			t.Fatal(err)
		}
		home, _ := os.UserHomeDir()
		if got != filepath.Join(home, ".looprig") {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("absolute override wins", func(t *testing.T) {
		dir := t.TempDir()
		got, err := looprigHome(Config{HomeDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if got != dir {
			t.Fatalf("got %q want %q", got, dir)
		}
	})
	t.Run("relative override rejected", func(t *testing.T) {
		if _, err := looprigHome(Config{HomeDir: "rel/path"}); err == nil {
			t.Fatal("want error for relative HomeDir")
		}
	})
}
