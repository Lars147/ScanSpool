//go:build windows

package output

import "os"

// Windows cannot sync directory handles; the PDF itself is flushed before
// publication, matching the platform limitation of internal persistence.
func syncOutputDirectory(*os.File) error { return nil }
