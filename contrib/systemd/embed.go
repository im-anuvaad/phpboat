// Package systemd embeds phpboat's systemd unit so the binary can install
// the exact file that ships in this directory (`phpboat setup-host`).
package systemd

import _ "embed"

// Unit is contrib/systemd/phpboat.service.
//
//go:embed phpboat.service
var Unit string

// UnitPath is where the unit is installed.
const UnitPath = "/etc/systemd/system/phpboat.service"
