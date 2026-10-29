// Package phpfile knows which file names look like PHP source.
package phpfile

import (
	"path/filepath"
	"strings"
)

// Like reports whether name has an extension PHP source is stored under
// (case-insensitive). phpserve never sends such a file as static content,
// and backups count the ones found in DATA_DIR. Only lowercase ".php" is
// executed (FPM's security.limit_extensions).
func Like(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".php", ".phtml", ".phar", ".php3", ".php4", ".php5", ".php7", ".php8", ".phps", ".inc":
		return true
	}
	return false
}
