package server

import (
	"net"
	"net/http"
	"strconv"

	"github.com/calper-ql/crosstalk/internal/dist"
	"github.com/calper-ql/crosstalk/internal/proto"
)

// BindPort tells the API which port the server listens on, for rendering
// install.sh when a proxy or odd client strips the port from the Host
// header. Set from config at startup.
var BindPort = strconv.Itoa(proto.DefaultPort)

// handleInstallScript serves /install.sh with this server's own address
// baked in, taken from the Host header the client actually reached us by.
func handleInstallScript(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		host, port = r.Host, BindPort
	}
	if host == "" {
		writeErr(w, http.StatusBadRequest, proto.ErrBadRequest, "cannot determine server address from request")
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Write([]byte(dist.InstallScript(host, port)))
}

// handleBinary serves the embedded client binaries and checksum manifest.
func handleBinary(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	data, ok := dist.Asset(name)
	if !ok {
		writeErr(w, http.StatusNotFound, proto.ErrNotFound,
			"this server build does not carry "+name+" — rebuild crosstalkd with the release matrix (run.sh)")
		return
	}
	if name == "SHA256SUMS" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Write(data)
}
