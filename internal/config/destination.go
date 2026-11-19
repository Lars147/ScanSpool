package config

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
)

const (
	KindFTP   = "ftp"
	KindLocal = "local"
)

// Destination embeds the legacy flat FTP settings so existing settings and
// queued jobs remain readable. Only the active destination is validated.
type Destination struct {
	FTP
	Kind           string `json:"kind,omitempty"`
	LocalDirectory string `json:"localDirectory,omitempty"`
}

func (d Destination) OutputKind() string {
	if d.Kind == "" {
		return KindFTP
	}
	return d.Kind
}

func (d Destination) Validate() error {
	switch d.OutputKind() {
	case KindFTP:
		return d.FTP.Validate()
	case KindLocal:
		if !filepath.IsAbs(d.LocalDirectory) || strings.IndexFunc(d.LocalDirectory, unicode.IsControl) >= 0 {
			return fmt.Errorf("enter an absolute local directory, for example /volume1/scans")
		}
		return nil
	default:
		return fmt.Errorf("unknown output kind %q", d.Kind)
	}
}

func (d Destination) SameTarget(other Destination) bool {
	if d.OutputKind() != other.OutputKind() {
		return false
	}
	switch d.OutputKind() {
	case KindFTP:
		return d.FTP.SameTarget(other.FTP)
	case KindLocal:
		a, b := filepath.Clean(d.LocalDirectory), filepath.Clean(other.LocalDirectory)
		return a == b || (runtime.GOOS == "windows" && strings.EqualFold(a, b))
	default:
		return false
	}
}

// ValidateLocalDirectory rejects the outbox and its descendants. Callers must
// also call this with resolved paths immediately before writing local output.
func ValidateLocalDirectory(directory, outbox string) error {
	root, err := filepath.Abs(outbox)
	if err != nil {
		return err
	}
	directory = filepath.Clean(directory)
	if runtime.GOOS == "windows" {
		root, directory = strings.ToLower(root), strings.ToLower(directory)
	}
	rel, err := filepath.Rel(root, directory)
	if err != nil {
		// Distinct Windows volumes cannot contain each other.
		if filepath.VolumeName(root) != filepath.VolumeName(directory) {
			return nil
		}
		return err
	}
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("local output must be separate from the internal outbox")
	}
	return nil
}
