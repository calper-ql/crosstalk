package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calper-ql/crosstalk/internal/dist"
	"github.com/calper-ql/crosstalk/internal/server"
)

// pinnedServer is a crosstalkd over TLS that also hands out the installer
// and the client built for this test — what a release build embeds — and
// remembers which paths were asked for with the token.
type pinnedServer struct {
	addr, port  string
	fingerprint string // sha256 of the certificate, hex: what `register` pins
	pin         string // sha256 of its public key, in curl's form
	mu          sync.Mutex
	withToken   []string
}

func startPinnedServer(t *testing.T) *pinnedServer {
	t.Helper()
	ps := &pinnedServer{}
	api := server.NewAPI(server.NewHub(server.Config{}), testToken).Handler()
	client, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	name := "crosstalk-" + runtime.GOOS + "-" + runtime.GOARCH
	sum := sha256.Sum256(client)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /install.sh", func(w http.ResponseWriter, r *http.Request) {
		host, port, _ := net.SplitHostPort(r.Host)
		w.Write([]byte(dist.InstallScript(host, port)))
	})
	mux.HandleFunc("GET /bin/"+name, func(w http.ResponseWriter, r *http.Request) { w.Write(client) })
	mux.HandleFunc("GET /bin/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), testToken) {
			ps.mu.Lock()
			ps.withToken = append(ps.withToken, r.URL.Path)
			ps.mu.Unlock()
		}
		api.ServeHTTP(w, r)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	ps.addr, ps.port, _ = net.SplitHostPort(srv.Listener.Addr().String())
	cert := srv.Certificate()
	fp := sha256.Sum256(cert.Raw)
	ps.fingerprint = hex.EncodeToString(fp[:])
	spki := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	ps.pin = "sha256//" + base64.StdEncoding.EncodeToString(spki[:])
	return ps
}

func (ps *pinnedServer) tokenUses() int {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return len(ps.withToken)
}

// runEnv runs the client in a home of its own with extra environment.
func runEnv(t *testing.T, homeDir string, env []string, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := home{homeDir}.cmd(ctx, args...)
	cmd.Env = append(cmd.Env, env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	return finish(t, cmd.Run(), &out, &errb)
}

// With --fingerprint there is no first use to trust: the server must show
// exactly that certificate, and the token is not sent to one that does not.
func TestRegisterPinsAGivenFingerprint(t *testing.T) {
	ps := startPinnedServer(t)

	// However a person or a tool writes it.
	colons := "sha256:"
	for i := 0; i < len(ps.fingerprint); i += 2 {
		if i > 0 {
			colons += ":"
		}
		colons += strings.ToUpper(ps.fingerprint[i : i+2])
	}
	for _, fp := range []string{ps.fingerprint, colons} {
		dir := t.TempDir()
		r := runEnv(t, dir, nil, "register", ps.addr, ps.port, "hub", "--token", testToken, "--fingerprint", fp)
		if r.code != 0 || !strings.Contains(r.stderr, "cert matches the given fingerprint") {
			t.Fatalf("register --fingerprint %s: exit %d\n%s", fp, r.code, r.stderr)
		}
		reg, _ := os.ReadFile(filepath.Join(dir, "servers.toml"))
		if !strings.Contains(string(reg), ps.fingerprint) {
			t.Errorf("the registry does not hold the pinned fingerprint:\n%s", reg)
		}
		if r := runEnv(t, dir, nil, "rooms", "hub"); r.code != 0 {
			t.Errorf("the registration does not work: %s", r.stderr)
		}
	}

	// Another certificate than the one named: refused, nothing written, and
	// the token never left this machine.
	before := ps.tokenUses()
	dir := t.TempDir()
	wrong := strings.Repeat("ab", 32)
	r := runEnv(t, dir, nil, "register", ps.addr, ps.port, "hub", "--token", testToken, "--fingerprint", wrong)
	if r.code == 0 || !strings.Contains(r.stderr, "presents a different certificate") || !strings.Contains(r.stderr, "the token was not sent") {
		t.Fatalf("register with the wrong fingerprint: exit %d\n%s", r.code, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "servers.toml")); err == nil {
		t.Error("a refused registration wrote the registry")
	}
	if ps.tokenUses() != before {
		t.Error("the token was sent to a server whose certificate was not the one named")
	}

	// Something that is not a fingerprint at all.
	for _, bad := range []string{"abc", strings.Repeat("zz", 32), "sha256:" + strings.Repeat("a", 63)} {
		if r := runEnv(t, t.TempDir(), nil, "register", ps.addr, ps.port, "hub", "--token", testToken, "--fingerprint", bad); r.code == 0 || !strings.Contains(r.stderr, "64 hex characters") {
			t.Errorf("--fingerprint %q: exit %d %s", bad, r.code, r.stderr)
		}
	}
}

// The token can come from the environment, so it need not be an argument.
func TestRegisterTakesTheTokenFromTheEnvironment(t *testing.T) {
	ps := startPinnedServer(t)
	dir := t.TempDir()
	r := runEnv(t, dir, []string{"CROSSTALK_TOKEN=" + testToken}, "register", ps.addr, ps.port, "hub")
	if r.code != 0 || !strings.Contains(r.stderr, "cert pinned on first use") {
		t.Fatalf("register with CROSSTALK_TOKEN: exit %d\n%s", r.code, r.stderr)
	}
	if r := runEnv(t, dir, nil, "rooms", "hub"); r.code != 0 {
		t.Errorf("the registration does not work: %s", r.stderr)
	}
	// A flag still wins over the environment, and neither is an error that says so.
	if r := runEnv(t, t.TempDir(), []string{"CROSSTALK_TOKEN=not-the-token"}, "register", ps.addr, ps.port, "hub", "--token", testToken); r.code != 0 {
		t.Errorf("--token did not win over CROSSTALK_TOKEN: %s", r.stderr)
	}
	if r := runEnv(t, t.TempDir(), nil, "register", ps.addr, ps.port, "hub"); r.code == 0 || !strings.Contains(r.stderr, "CROSSTALK_TOKEN") {
		t.Errorf("register with no token at all: exit %d %s", r.code, r.stderr)
	}
}

// The installer, run for real with curl and sh: pinned from the bootstrap
// fetch to the registration, the token in the environment — and refused
// outright when the server is not the one the pin names.
func TestInstallerPinnedEndToEnd(t *testing.T) {
	for _, tool := range []string{"sh", "curl", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	ps := startPinnedServer(t)
	base := "https://" + net.JoinHostPort(ps.addr, ps.port)

	install := func(homeDir, pin, fingerprint string) (string, error) {
		line := fmt.Sprintf(`curl -fsSLk --pinnedpubkey '%s' %s/install.sh | sh -s -- --alias hub --fingerprint %s --pin '%s'`,
			pin, base, fingerprint, pin)
		cmd := exec.Command("sh", "-c", line)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + homeDir, "CROSSTALK_TOKEN=" + testToken}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	homeDir := t.TempDir()
	out, err := install(homeDir, ps.pin, ps.fingerprint)
	if err != nil || !strings.Contains(out, `==> connected: crosstalk`) {
		t.Fatalf("the pinned install failed: %v\n%s", err, out)
	}
	if strings.Contains(out, testToken) {
		t.Errorf("the installer printed the token:\n%s", out)
	}
	installed := filepath.Join(homeDir, ".local", "bin", "crosstalk")
	if _, err := os.Stat(installed); err != nil {
		t.Fatalf("the client was not installed: %v", err)
	}
	reg, _ := os.ReadFile(filepath.Join(homeDir, ".crosstalk", "servers.toml"))
	if !strings.Contains(string(reg), ps.fingerprint) || !strings.Contains(string(reg), testToken) {
		t.Errorf("the registry after the install:\n%s", reg)
	}

	// A pin that names another key: curl refuses the server before a byte of
	// the script is run, so nothing is installed and nothing registered.
	other := sha256.Sum256([]byte("some other key"))
	wrongPin := "sha256//" + base64.StdEncoding.EncodeToString(other[:])
	homeDir = t.TempDir()
	before := ps.tokenUses()
	out, _ = install(homeDir, wrongPin, ps.fingerprint)
	if strings.Contains(out, "==> connected") || strings.Contains(out, "==> installed") {
		t.Fatalf("an install against the wrong key went ahead:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(homeDir, ".local")); err == nil {
		t.Error("something was installed although the server's key was not the pinned one")
	}
	if ps.tokenUses() != before {
		t.Error("the token was sent although the server's key was not the pinned one")
	}

	// The right key for the downloads but the wrong certificate named for the
	// registration: the client is put in place, and registration stops there.
	homeDir = t.TempDir()
	before = ps.tokenUses()
	out, err = install(homeDir, ps.pin, strings.Repeat("cd", 32))
	if err == nil || !strings.Contains(out, "presents a different certificate") {
		t.Fatalf("a registration against the wrong fingerprint: %v\n%s", err, out)
	}
	if ps.tokenUses() != before {
		t.Error("the token was sent to a server whose certificate was not the one named")
	}
}
