// crosstalkd is the nexus for all crosstalk conversations: a single static
// binary serving the API (and, in Phase 3, client binaries and the install
// script) on one mandatory, explicitly configured ip:port.
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/calper-ql/crosstalk/internal/proto"
	"github.com/calper-ql/crosstalk/internal/server"
)

type fileConfig struct {
	// Bind is mandatory with no default: exposing the server wider than
	// intended must be a deliberate act.
	Bind  string `toml:"bind"`
	Token string `toml:"token"`
	// StateDir holds the TLS cert/key; defaults to the config file's dir.
	StateDir string `toml:"state_dir"`

	// The operator (optional, both or neither): a second token, held by one
	// front end that speaks for signed-in people, and the hostname only that
	// token may join under. See server.API.
	OperatorToken    string `toml:"operator_token"`
	OperatorHostname string `toml:"operator_hostname"`

	// Phase 5 tunables; zero means the documented default.
	RoomCap        int     `toml:"room_cap"`
	IdleExpiryMins int     `toml:"idle_expiry_minutes"`
	RatePerMin     float64 `toml:"rate_per_minute"`
	Burst          float64 `toml:"burst"`
	MaxBodyKiB     int     `toml:"max_body_kib"`
	MaxWaitSecs    int     `toml:"max_wait_seconds"`

	// Files a message may carry, all held in memory (server/files.go): the
	// largest one (negative: take no files), what one room's may add up to,
	// and what all rooms' may. Zero means the default: 25, 128, 512.
	MaxFileMiB    int `toml:"max_file_mib"`
	RoomFilesMiB  int `toml:"room_files_mib"`
	TotalFilesMiB int `toml:"total_files_mib"`
}

func main() {
	configPath := flag.String("config", "crosstalkd.toml", "path to config file")
	flag.Parse()

	var cfg fileConfig
	if _, err := toml.DecodeFile(*configPath, &cfg); err != nil {
		fatalf("config: %v", err)
	}
	if cfg.Bind == "" {
		fatalf("config: bind is mandatory (e.g. bind = \"127.0.0.1:%d\") — there is deliberately no default", proto.DefaultPort)
	}
	if cfg.Token == "" {
		fatalf("config: token is mandatory — generate one with e.g. `openssl rand -hex 24`")
	}
	switch {
	case (cfg.OperatorToken == "") != (cfg.OperatorHostname == ""):
		fatalf("config: operator_token and operator_hostname go together — set both or neither")
	case cfg.OperatorToken != "" && cfg.OperatorToken == cfg.Token:
		fatalf("config: operator_token must differ from token — the fleet shares that one, which is the point of having a second")
	case cfg.OperatorHostname != "" && !server.ValidName(cfg.OperatorHostname):
		fatalf("config: operator_hostname must be 1-64 chars of letters, digits, . _ -")
	}
	if cfg.StateDir == "" {
		abs, err := filepath.Abs(*configPath)
		if err != nil {
			fatalf("config: %v", err)
		}
		cfg.StateDir = filepath.Dir(abs)
	}

	cert, err := server.LoadOrCreateCert(cfg.StateDir)
	if err != nil {
		fatalf("tls: %v", err)
	}

	hub := server.NewHub(server.Config{
		RoomCap:    cfg.RoomCap,
		IdleExpiry: time.Duration(cfg.IdleExpiryMins) * time.Minute,
		RatePerMin: cfg.RatePerMin,
		Burst:      cfg.Burst,
		MaxBody:    cfg.MaxBodyKiB * 1024,
		MaxWait:    time.Duration(cfg.MaxWaitSecs) * time.Second,
		MaxFile:    cfg.MaxFileMiB << 20,
		RoomFiles:  int64(cfg.RoomFilesMiB) << 20,
		TotalFiles: int64(cfg.TotalFilesMiB) << 20,
	})
	stop := make(chan struct{})
	go hub.RunGC(stop, log.Printf)
	defer close(stop)

	if _, port, err := net.SplitHostPort(cfg.Bind); err == nil {
		server.BindPort = port
	}
	api := server.NewAPI(hub, cfg.Token)
	api.OperatorToken, api.OperatorHostname = cfg.OperatorToken, cfg.OperatorHostname
	srv := &http.Server{
		Addr:    cfg.Bind,
		Handler: api.Handler(),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
		ReadHeaderTimeout: 10 * time.Second,
		// Long polls hold up to MaxWait (30s default); give writes headroom.
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	log.Printf("crosstalkd %s listening on %s", proto.FullVersion(), cfg.Bind)
	log.Printf("certificate fingerprint: %s", server.FormatFingerprint(server.Fingerprint(cert)))
	if cfg.OperatorHostname != "" {
		log.Printf("operator: the hostname %q is reserved for the operator token", cfg.OperatorHostname)
	}
	if n := hub.MaxFile(); n > 0 {
		log.Printf("files: up to %d MiB each, held in memory only", n>>20)
	} else {
		log.Printf("files: off (max_file_mib is negative)")
	}

	// The copyable lines carry the token, so they go to a person at a
	// terminal and nowhere else: under a service manager or in a container,
	// stderr is a log, and a log is no place for the fleet's one secret.
	if stderrIsTerminal() {
		host, port := advertiseAddr(cfg.Bind)
		fmt.Fprintf(os.Stderr, "\ninstall a client on any machine with:\n\n"+
			"  curl -fsSLk https://%s:%s/install.sh | sh -s -- --token %s --alias hub\n\n"+
			"or register an existing client:\n\n"+
			"  crosstalk register %s %s hub --token %s\n\n",
			host, port, cfg.Token, host, port, cfg.Token)
	} else {
		log.Printf("not a terminal: the install line (it carries the token) is not printed; the token is in %s", *configPath)
	}

	if err := srv.ListenAndServeTLS("", ""); err != nil {
		fatalf("serve: %v", err)
	}
}

// advertiseAddr turns the bind address into something another machine can
// actually reach: a wildcard host is replaced by this machine's first
// non-loopback IPv4 address.
func advertiseAddr(bind string) (host, port string) {
	host, port, err := net.SplitHostPort(bind)
	if err != nil {
		return bind, strconv.Itoa(proto.DefaultPort)
	}
	if ip := net.ParseIP(host); host != "" && (ip == nil || !ip.IsUnspecified()) {
		return host, port
	}
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
				return ipn.IP.String(), port
			}
		}
	}
	if h, err := os.Hostname(); err == nil {
		return h, port
	}
	return "127.0.0.1", port
}

// stderrIsTerminal reports whether stderr is a character device — a
// terminal, as opposed to the pipe or file a supervisor reads logs from.
func stderrIsTerminal() bool {
	st, err := os.Stderr.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func fatalf(format string, v ...any) {
	fmt.Fprintf(os.Stderr, "crosstalkd: "+format+"\n", v...)
	os.Exit(1)
}
