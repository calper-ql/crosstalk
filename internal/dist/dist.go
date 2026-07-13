// Package dist embeds the cross-compiled client binaries and checksum
// manifest into crosstalkd — what makes single-artifact deployment work:
// ship one crosstalkd, and every client that can reach its ip:port can
// bootstrap itself from it.
//
// The assets directory is populated by cli.sh (or a release build) before
// the server is compiled. A dev server built without the matrix serves 404
// for missing binaries but still works fully as a hub.
package dist

import (
	"embed"
	"strings"
)

//go:embed all:assets
var assets embed.FS

//go:embed install.sh.tmpl
var installTemplate string

// Asset returns an embedded distribution file (crosstalk-{os}-{arch} or
// SHA256SUMS) or false if this build doesn't carry it.
func Asset(name string) ([]byte, bool) {
	if strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return nil, false
	}
	data, err := assets.ReadFile("assets/" + name)
	if err != nil {
		return nil, false
	}
	return data, true
}

// InstallScript renders install.sh with the server's host and port baked
// in, so the bootstrap and the registration can never disagree about the
// address.
func InstallScript(host, port string) string {
	s := strings.ReplaceAll(installTemplate, "__SERVER_HOST__", host)
	return strings.ReplaceAll(s, "__SERVER_PORT__", port)
}
