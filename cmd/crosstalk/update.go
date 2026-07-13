package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/calper-ql/crosstalk/internal/client"
	"github.com/calper-ql/crosstalk/internal/proto"
)

// cmdUpdate compares the embedded build version against the server's and
// replaces this binary in place when they differ. Runs over the pinned TLS
// connection, verifying the checksum manifest — day-to-day updates don't
// need curl.
func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	alias := ""
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		alias, args = args[0], args[1:]
	}
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}

	reg, err := client.LoadRegistry()
	if err != nil {
		return err
	}
	alias, entry, err := reg.Lookup(alias)
	if err != nil {
		return err
	}
	conn := client.NewConn(entry)

	ver, err := conn.Version()
	if err != nil {
		return fmt.Errorf("update: cannot reach %q: %w", alias, err)
	}
	// Compare the full build identity, not just the version number: a
	// rebuilt server must be able to push new clients within one version.
	if ver.Version == proto.Version && (ver.Build == "" || ver.Build == proto.Build) {
		fmt.Fprintf(os.Stderr, "already up to date (%s)\n", proto.FullVersion())
		return nil
	}

	name := fmt.Sprintf("crosstalk-%s-%s", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(os.Stderr, "updating %s (build %s) -> %s (build %s) [%s]\n",
		proto.Version, proto.Build, ver.Version, ver.Build, name)

	binData, err := conn.Download("/bin/" + name)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	sums, err := conn.Download("/bin/SHA256SUMS")
	if err != nil {
		return fmt.Errorf("update: fetching checksum manifest: %w", err)
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == name {
			want = f[0]
		}
	}
	if want == "" {
		return fmt.Errorf("update: %s missing from the server's checksum manifest", name)
	}
	sum := sha256.Sum256(binData)
	if hex.EncodeToString(sum[:]) != want {
		return fmt.Errorf("update: checksum mismatch for %s — refusing to install", name)
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return err
	}
	// Write next to the target and rename over it: atomic on the same
	// filesystem, and the running process keeps its old inode.
	tmp := self + ".new"
	if err := os.WriteFile(tmp, binData, 0755); err != nil {
		return err
	}
	if err := os.Rename(tmp, self); err != nil {
		os.Remove(tmp)
		return err
	}
	fmt.Fprintf(os.Stderr, "updated %s -> %s\n", self, ver.Version)
	return nil
}
