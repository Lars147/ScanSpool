package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode"
)

// LocalDirectoryError distinguishes destination access failures from permission
// errors involving internal state, scanner sockets or unrelated files.
type LocalDirectoryError struct {
	Directory string
	Err       error
}

func (e *LocalDirectoryError) Error() string {
	return fmt.Sprintf("local output folder %q: %v", e.Directory, e.Err)
}
func (e *LocalDirectoryError) Unwrap() error { return e.Err }

// LocalPermissionHint describes a manual repair on the receiver host. It never
// executes commands or changes permissions, and makes no NAS-specific assumptions
// when the standard Linux ACL tool is available.
func LocalPermissionHint(err error) string {
	var local *LocalDirectoryError
	if !errors.Is(err, os.ErrPermission) || !errors.As(err, &local) {
		return ""
	}
	uid := strconv.Itoa(os.Geteuid())
	account, lookupErr := user.LookupId(uid)
	username := "UID " + uid
	if lookupErr == nil {
		username = account.Username
	}
	tool := ""
	if runtime.GOOS == "linux" {
		const synologyTool = "/usr/syno/bin/synoacltool"
		if info, statErr := os.Stat(synologyTool); statErr == nil && !info.IsDir() && info.Mode().Perm()&0111 != 0 {
			tool = synologyTool
		} else if path, pathErr := exec.LookPath("setfacl"); pathErr == nil {
			tool = path
		}
	}
	if lookupErr != nil && filepath.Base(tool) == "synoacltool" {
		// Synology needs an actual account name; generic setfacl accepts UIDs.
		tool = ""
	}
	return localPermissionHint(username, uid, local.Directory, tool)
}

func localPermissionHint(username, uid, directory, tool string) string {
	intro := "Permission denied. Check file and folder access rights."
	if (Destination{Kind: KindLocal, LocalDirectory: directory}).Validate() != nil || strings.Contains(username, ":") || strings.IndexFunc(username, unicode.IsControl) >= 0 || username == "" {
		return intro
	}
	var aclCommand string
	switch filepath.Base(tool) {
	case "synoacltool":
		entry := "user:" + username + ":allow:rwxpdDaARWc--:fd--"
		aclCommand = "sudo " + shellQuote(tool) + " -add \\\n  " + shellQuote(directory) + " \\\n  " + shellQuote(entry)
	case "setfacl":
		if id, err := strconv.Atoi(uid); err != nil || id < 0 {
			return intro
		}
		aclCommand = "sudo " + shellQuote(tool) + " -m " + shellQuote("u:"+uid+":rwx") + " -- \\\n  " + shellQuote(directory)
	default:
		return intro + "\nGrant receiver user " + shellQuote(username) + " read/write access to the output folder."
	}
	return intro + "\nRun on receiver as administrator (user " + username + "):\n" +
		"sudo mkdir -p -- " + shellQuote(directory) + "\n" + aclCommand + "\nThen retry queued deliveries."
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
