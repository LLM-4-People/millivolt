//go:build tools

// Package millivolt carries the tool dependencies that are not imported by the
// proxy itself. The build tag keeps them out of the binary while go.mod, go.sum
// and the module graph pin them: the security workflow runs govulncheck from
// this module instead of resolving a version at run time, so the scanner that
// judges the tree is itself pinned and checksummed here.
package millivolt

import _ "golang.org/x/vuln/cmd/govulncheck"
