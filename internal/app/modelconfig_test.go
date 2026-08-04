package app

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDefaultModelConfigPath(t *testing.T) {
	home := t.TempDir()
	setProcessHome(t, home)

	tests := []struct {
		name      string
		workspace string
	}{
		{name: "first workspace", workspace: filepath.Join(t.TempDir(), "first")},
		{name: "second workspace", workspace: filepath.Join(t.TempDir(), "second")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := defaultModelConfigPath()
			if err != nil {
				t.Fatalf("defaultModelConfigPath() error = %v", err)
			}
			want := filepath.Join(home, ".looprig", "models.json")
			if got != want {
				t.Errorf("defaultModelConfigPath() = %q, want %q", got, want)
			}
			if strings.Contains(got, string(filepath.Separator)+"workspaces"+string(filepath.Separator)) {
				t.Errorf("defaultModelConfigPath() = %q, must not contain a workspaces segment", got)
			}

			permissionsPath, err := defaultPermissionsPath(tt.workspace)
			if err != nil {
				t.Fatalf("defaultPermissionsPath(%q) error = %v", tt.workspace, err)
			}
			if got == permissionsPath {
				t.Errorf("defaultModelConfigPath() = defaultPermissionsPath(%q) = %q", tt.workspace, got)
			}
		})
	}

	t.Run("home lookup failure is typed", func(t *testing.T) {
		setProcessHome(t, "")
		_, err := defaultModelConfigPath()
		var configErr *ModelConfigError
		if !errors.As(err, &configErr) {
			t.Fatalf("defaultModelConfigPath() error = %T %v, want *ModelConfigError", err, err)
		}
	})
}

func TestReadModelConfigFile(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		got, exists, err := readModelConfigFile(filepath.Join(t.TempDir(), "missing.json"))
		if err != nil || exists || got != nil {
			t.Fatalf("readModelConfigFile(absent) = (%q, %v, %v), want (nil, false, nil)", got, exists, err)
		}
	})

	t.Run("long failing path has bounded error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), strings.Repeat("long-component-", 128))
		_, _, err := readModelConfigFile(path)
		if err == nil {
			t.Fatal("readModelConfigFile(long path) error = nil, want error")
		}
		if len(err.Error()) > 512 {
			t.Errorf("readModelConfigFile(long path) error length = %d, want <= 512", len(err.Error()))
		}
		var configErr *ModelConfigError
		if !errors.As(err, &configErr) {
			t.Errorf("error type = %T, want *ModelConfigError", err)
		}
	})

	t.Run("regular owner-only file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "models.json")
		want := []byte(`{"version":2}`)
		writeModelConfigFixture(t, path, want, 0o600)

		got, exists, err := readModelConfigFile(path)
		if err != nil {
			t.Fatalf("readModelConfigFile() error = %v", err)
		}
		if !exists {
			t.Fatal("readModelConfigFile() exists = false, want true")
		}
		if string(got) != string(want) {
			t.Errorf("readModelConfigFile() = %q, want %q", got, want)
		}
	})

	t.Run("directory", func(t *testing.T) {
		assertModelConfigReadRejected(t, t.TempDir())
	})

	t.Run("final-path symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target.json")
		writeModelConfigFixture(t, target, []byte(`{"target":true}`), 0o600)
		path := filepath.Join(dir, "models.json")
		if err := os.Symlink(target, path); err != nil {
			t.Skipf("symlinks unsupported: %v", err)
		}
		assertModelConfigReadRejected(t, path)
	})

	t.Run("non-regular type", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "models.sock")
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Skipf("Unix-domain sockets unsupported: %v", err)
		}
		defer listener.Close()
		assertModelConfigReadRejected(t, path)
	})

	t.Run("over size limit", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "models.json")
		content := make([]byte, maxModelConfigBytes+1)
		writeModelConfigFixture(t, path, content, 0o600)
		assertModelConfigReadRejected(t, path)
	})

	t.Run("exact size limit", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "models.json")
		want := bytes.Repeat([]byte{'x'}, maxModelConfigBytes)
		writeModelConfigFixture(t, path, want, 0o600)

		got, exists, err := readModelConfigFile(path)
		if err != nil || !exists || !bytes.Equal(got, want) {
			t.Fatalf("readModelConfigFile(exact limit) = (%d bytes, %v, %v), want (%d bytes, true, nil)", len(got), exists, err, len(want))
		}
	})

	t.Run("replacement symlink is never followed", func(t *testing.T) {
		if !modelConfigNoFollowTestSupported() {
			t.Skip("platform has no native final-component no-follow open")
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "models.json")
		target := filepath.Join(dir, "target.json")
		writeModelConfigFixture(t, path, []byte(`{"safe":true}`), 0o600)
		writeModelConfigFixture(t, target, []byte(`{"secret":true}`), 0o600)

		followed := false
		got, exists, err := readModelConfigFileWithOpen(path, func(openPath string) (*os.File, error) {
			if removeErr := os.Remove(openPath); removeErr != nil {
				return nil, removeErr
			}
			if symlinkErr := os.Symlink(target, openPath); symlinkErr != nil {
				return nil, symlinkErr
			}
			file, openErr := openModelConfigNoFollow(openPath)
			if openErr == nil {
				info, statErr := file.Stat()
				followed = statErr == nil && info.Mode().IsRegular()
			}
			return file, openErr
		})
		if err == nil || !exists || got != nil {
			t.Fatalf("read after replacement symlink = (%q, %v, %v), want nil, true, error", got, exists, err)
		}
		if followed {
			t.Fatal("openModelConfigNoFollow followed the replacement symlink")
		}
	})

	t.Run("opened handle permissions are rechecked", func(t *testing.T) {
		if !modelConfigTestIsUnix() {
			t.Skip("Unix permission bits are not supported on this platform")
		}
		path := filepath.Join(t.TempDir(), "models.json")
		writeModelConfigFixture(t, path, []byte(`{"safe":true}`), 0o600)

		got, exists, err := readModelConfigFileWithOpen(path, func(openPath string) (*os.File, error) {
			if chmodErr := os.Chmod(openPath, 0o640); chmodErr != nil {
				return nil, chmodErr
			}
			return openModelConfigNoFollow(openPath)
		})
		if err == nil || !exists || got != nil {
			t.Fatalf("read after permission change = (%q, %v, %v), want nil, true, error", got, exists, err)
		}
	})

	t.Run("Unix permissions", func(t *testing.T) {
		if !modelConfigTestIsUnix() {
			t.Skip("Unix permission bits are not supported on this platform")
		}
		for _, tt := range []struct {
			name string
			mode os.FileMode
			ok   bool
		}{
			{name: "owner only", mode: 0o600, ok: true},
			{name: "group readable", mode: 0o640},
			{name: "other readable", mode: 0o604},
			{name: "world writable", mode: 0o666},
		} {
			t.Run(tt.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "models.json")
				writeModelConfigFixture(t, path, []byte(`{"safe":true}`), tt.mode)
				got, exists, err := readModelConfigFile(path)
				if tt.ok {
					if err != nil || !exists || string(got) != `{"safe":true}` {
						t.Fatalf("readModelConfigFile(mode %04o) = (%q, %v, %v), want exact bytes, true, nil", tt.mode.Perm(), got, exists, err)
					}
					return
				}
				if err == nil || !exists || got != nil {
					t.Fatalf("readModelConfigFile(mode %04o) = (%q, %v, %v), want nil, true, error", tt.mode.Perm(), got, exists, err)
				}
			})
		}
	})

	t.Run("read error is wrapped and redacted", func(t *testing.T) {
		if !modelConfigTestIsUnix() {
			t.Skip("Unix permission bits are not supported on this platform")
		}
		path := filepath.Join(t.TempDir(), "models.json")
		const secret = "MODEL_CONFIG_SECRET_SENTINEL"
		writeModelConfigFixture(t, path, []byte(secret), 0o000)
		defer os.Chmod(path, 0o600)

		got, exists, err := readModelConfigFile(path)
		if err == nil {
			t.Skip("current user can read mode 000 files")
		}
		if got != nil || !exists {
			t.Fatalf("readModelConfigFile(unreadable) = (%q, %v, %v), want nil, true, error", got, exists, err)
		}
		var configErr *ModelConfigError
		if !errors.As(err, &configErr) {
			t.Errorf("error type = %T, want *ModelConfigError", err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error exposed file contents: %v", err)
		}
	})
}

func writeModelConfigFixture(t *testing.T, path string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod fixture to %04o: %v", mode.Perm(), err)
	}
}

func assertModelConfigReadRejected(t *testing.T, path string) {
	t.Helper()
	got, exists, err := readModelConfigFile(path)
	if err == nil || !exists || got != nil {
		t.Fatalf("readModelConfigFile(%q) = (%q, %v, %v), want nil, true, error", path, got, exists, err)
	}
}

func modelConfigTestIsUnix() bool {
	switch runtime.GOOS {
	case "aix", "android", "darwin", "dragonfly", "freebsd", "hurd", "illumos", "ios", "linux", "netbsd", "openbsd", "solaris":
		return true
	default:
		return false
	}
}

func modelConfigNoFollowTestSupported() bool {
	return modelConfigTestIsUnix() || runtime.GOOS == "windows"
}

func setProcessHome(t *testing.T, home string) {
	t.Helper()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("USERPROFILE", home)
		t.Setenv("HOMEDRIVE", "")
		t.Setenv("HOMEPATH", "")
	case "plan9":
		t.Setenv("home", home)
	default:
		t.Setenv("HOME", home)
	}
}
