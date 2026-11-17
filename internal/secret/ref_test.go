package secret

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bgfdev/taut/internal/fail"
)

func failCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var fe *fail.Error
	if !errors.As(err, &fe) {
		t.Fatalf("error is %T, want *fail.Error: %v", err, err)
	}
	if fe.What == "" || fe.Next == "" {
		t.Errorf("error %q lacks What or Next: %+v", fe.Code, fe)
	}
	return fe.Code
}

func TestRefValidate(t *testing.T) {
	tests := []struct {
		name string
		ref  Ref
		code string
	}{
		{"env", Ref{Env: "TAUT_SLACK_TOKEN"}, ""},
		{"file", Ref{File: "./taut-app.pem"}, ""},
		{"absolute file", Ref{File: "/run/secrets/slack"}, ""},
		{"empty", Ref{}, "secret.ref_invalid"},
		{"both", Ref{Env: "A", File: "b"}, "secret.ref_invalid"},
		{"bad env name", Ref{Env: "1BAD-NAME"}, "secret.ref_invalid"},
		{"env with spaces", Ref{Env: "MY TOKEN"}, "secret.ref_invalid"},
		{"literal slack token in env", Ref{Env: fakeSlackBot}, "secret.ref_literal"},
		{"literal jwt in file", Ref{File: fakeJWT}, "secret.ref_literal"},
		{"literal entra secret in env", Ref{Env: fakeEntra}, "secret.ref_literal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.ref.Validate()
			if got := failCode(t, err); got != tt.code {
				t.Fatalf("code = %q, want %q (%v)", got, tt.code, err)
			}
			if err != nil {
				for _, s := range []string{fakeSlackBot, fakeJWT, fakeEntra} {
					if strings.Contains(err.Error(), s) {
						t.Errorf("error echoes the secret: %v", err)
					}
				}
			}
		})
	}
}

func TestRefIsZeroAndDescribe(t *testing.T) {
	if !(Ref{}).IsZero() || (Ref{Env: "X"}).IsZero() || (Ref{File: "x"}).IsZero() {
		t.Error("IsZero is wrong")
	}
	tests := []struct {
		ref  Ref
		want string
	}{
		{Ref{Env: "TAUT_SLACK_TOKEN"}, "env TAUT_SLACK_TOKEN"},
		{Ref{File: "./taut-app.pem"}, "file ./taut-app.pem"},
		{Ref{}, "unset"},
		{Ref{Env: fakeSlackBot}, "env " + Marker},
		{Ref{File: fakeEntra}, "file " + Marker},
	}
	for _, tt := range tests {
		if got := tt.ref.Describe(); got != tt.want {
			t.Errorf("Describe(%+v) = %q, want %q", tt.ref, got, tt.want)
		}
	}
}

func TestRefResolveEnv(t *testing.T) {
	t.Setenv("TAUT_TEST_SECRET", "  hunter2-value\r\n")
	t.Setenv("TAUT_TEST_BLANK", " \n")

	v, warns, err := Ref{Env: "TAUT_TEST_SECRET"}.ResolveWithWarnings("")
	if err != nil {
		t.Fatal(err)
	}
	if v.Reveal() != "hunter2-value" {
		t.Errorf("Reveal() = %q", v.Reveal())
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}

	_, err = Ref{Env: "TAUT_TEST_BLANK"}.Resolve("")
	if got := failCode(t, err); got != "secret.empty" {
		t.Errorf("blank env: code = %q", got)
	}

	_, err = Ref{Env: "TAUT_TEST_DEFINITELY_UNSET_VARIABLE"}.Resolve("")
	if got := failCode(t, err); got != "secret.env_unset" {
		t.Errorf("unset env: code = %q", got)
	}
	if !strings.Contains(err.Error(), "TAUT_TEST_DEFINITELY_UNSET_VARIABLE") {
		t.Errorf("error does not name the variable: %v", err)
	}

	_, err = Ref{}.Resolve("")
	if got := failCode(t, err); got != "secret.ref_invalid" {
		t.Errorf("empty ref: code = %q", got)
	}
}

func TestRefResolveFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	abs := write("private", "file-secret\n\n", 0o600)
	write("blank", "\n", 0o600)
	write("shared", "shared-secret\n", 0o644)

	t.Run("relative to base dir", func(t *testing.T) {
		v, warns, err := Ref{File: "private"}.ResolveWithWarnings(dir)
		if err != nil {
			t.Fatal(err)
		}
		if v.Reveal() != "file-secret" {
			t.Errorf("Reveal() = %q", v.Reveal())
		}
		if len(warns) != 0 {
			t.Errorf("unexpected warnings for a 0600 file: %v", warns)
		}
	})
	t.Run("absolute ignores base dir", func(t *testing.T) {
		v, err := Ref{File: abs}.Resolve("/nonexistent-base")
		if err != nil || v.Reveal() != "file-secret" {
			t.Fatalf("got %q, %v", v.Reveal(), err)
		}
	})
	t.Run("empty file", func(t *testing.T) {
		_, err := Ref{File: "blank"}.Resolve(dir)
		if got := failCode(t, err); got != "secret.empty" {
			t.Errorf("code = %q", got)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		_, err := Ref{File: "nope"}.Resolve(dir)
		if got := failCode(t, err); got != "secret.file_not_found" {
			t.Errorf("code = %q", got)
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Error("error does not wrap fs.ErrNotExist")
		}
	})
	t.Run("directory", func(t *testing.T) {
		_, err := Ref{File: "."}.Resolve(dir)
		if got := failCode(t, err); got != "secret.file_unreadable" {
			t.Errorf("code = %q", got)
		}
	})
	t.Run("group/other readable warns", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("permission bits are not meaningful on Windows")
		}
		v, warns, err := Ref{File: "shared"}.ResolveWithWarnings(dir)
		if err != nil {
			t.Fatal(err)
		}
		if v.Reveal() != "shared-secret" {
			t.Errorf("Reveal() = %q", v.Reveal())
		}
		if len(warns) != 1 || warns[0].Code != WarnFilePermissions {
			t.Fatalf("warnings = %+v, want one %s", warns, WarnFilePermissions)
		}
		if s := warns[0].String(); !strings.Contains(s, "chmod 600") || strings.Contains(s, "shared-secret") {
			t.Errorf("warning text = %q", s)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("cannot make a file unreadable for this user")
		}
		write("locked", "x", 0o000)
		_, err := Ref{File: "locked"}.Resolve(dir)
		if got := failCode(t, err); got != "secret.file_unreadable" {
			t.Errorf("code = %q", got)
		}
		if fail.ExitCode(err) != fail.ExitPermission {
			t.Errorf("exit = %d, want %d", fail.ExitCode(err), fail.ExitPermission)
		}
	})
}
