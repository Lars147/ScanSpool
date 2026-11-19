package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxAndSynologyHintsUseReceiverIdentityAndExactFolder(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "O'Brien $(printf injection) `printf injection`; output")
	for _, tool := range []string{"/usr/bin/setfacl", "/usr/syno/bin/synoacltool"} {
		hint := localPermissionHint("receiver-account", "2042", directory, tool)
		for _, expected := range []string{"Permission denied", "Run on receiver", "receiver-account", shellQuote(directory), shellQuote(tool), "Then retry"} {
			if !strings.Contains(hint, expected) {
				t.Fatalf("missing %q in %s", expected, hint)
			}
		}
		if strings.Contains(hint, "chmod") || strings.Contains(hint, "chown") || strings.Contains(hint, " -R ") || strings.Contains(hint, " -del ") {
			t.Fatal("repair replaces ownership or broad permissions", hint)
		}
		if filepath.Base(tool) == "setfacl" {
			if !strings.Contains(hint, "'u:2042:rwx'") || strings.Contains(hint, "synoacltool") {
				t.Fatal("non-generic Linux hint", hint)
			}
		} else if !strings.Contains(hint, "'user:receiver-account:allow:rwxpdDaARWc--:fd--'") {
			t.Fatal("wrong Synology user ACL", hint)
		}
	}
}

func TestRepairHintDoesNotGuessCommandsWithoutToolOrIdentity(t *testing.T) {
	directory := t.TempDir()
	for _, hint := range []string{
		localPermissionHint("receiver", "2042", directory, ""),
		localPermissionHint("receiver", "2042", directory, "/bin/unknown-tool"),
		localPermissionHint("bad:identity", "2042", directory, "/usr/syno/bin/synoacltool"),
		localPermissionHint("receiver", "not-a-uid", directory, "/usr/bin/setfacl"),
	} {
		if strings.Contains(hint, "sudo") {
			t.Fatal("unverified repair command", hint)
		}
	}
}

func TestLinuxACLHintSupportsNumericIdentityWithoutPasswdEntry(t *testing.T) {
	hint := localPermissionHint("UID 2042", "2042", t.TempDir(), "/usr/bin/setfacl")
	if !strings.Contains(hint, "'u:2042:rwx'") || !strings.Contains(hint, "user UID 2042") {
		t.Fatal("unmapped Linux UID lost repair guidance", hint)
	}
}

func TestRepairHintOnlyAppliesToLocalDestinationPermissionErrors(t *testing.T) {
	permission := &os.PathError{Op: "open", Path: "internal-state", Err: os.ErrPermission}
	for _, err := range []error{nil, permission, &LocalDirectoryError{Directory: t.TempDir(), Err: os.ErrNotExist}} {
		if hint := LocalPermissionHint(err); hint != "" {
			t.Fatal("misleading output repair hint", err, hint)
		}
	}
	err := fmt.Errorf("queued PDF: %w", &LocalDirectoryError{Directory: t.TempDir(), Err: permission})
	if !errors.Is(err, os.ErrPermission) || !strings.Contains(LocalPermissionHint(err), "Permission denied") {
		t.Fatal("lost nested permission cause", err)
	}
}

func TestShellQuotePreservesSpecialCharactersWithoutEvaluation(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("POSIX shell unavailable")
	}
	value := "folder/O'Brien $(printf injected) `printf injected`; spaced"
	output, err := exec.Command(shell, "-c", "printf '%s' "+shellQuote(value)).Output()
	if err != nil || string(output) != value {
		t.Fatal("shell quoting changed or evaluated argument", string(output), err)
	}
}
