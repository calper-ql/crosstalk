// Package client implements the crosstalk client's local state (server
// registry, sessions, profile) and its pinned-TLS HTTP transport.
package client

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// ConfigDir returns ~/.crosstalk, honoring CROSSTALK_HOME for tests.
func ConfigDir() (string, error) {
	if d := os.Getenv("CROSSTALK_HOME"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".crosstalk"), nil
}

// ServerEntry is one registered server: alias → address, port, token, and
// the certificate fingerprint pinned at registration.
type ServerEntry struct {
	Addr        string `toml:"addr"`
	Port        int    `toml:"port"`
	Token       string `toml:"token"`
	Fingerprint string `toml:"fingerprint"` // sha256 hex of the DER leaf
}

// Registry is servers.toml: written by `crosstalk register`, mode 0600
// since it holds the token.
type Registry struct {
	Default string                 `toml:"default"`
	Servers map[string]ServerEntry `toml:"servers"`
}

var aliasRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func ValidAlias(a string) bool { return aliasRe.MatchString(a) }

func registryPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "servers.toml"), nil
}

func LoadRegistry() (*Registry, error) {
	path, err := registryPath()
	if err != nil {
		return nil, err
	}
	reg := &Registry{Servers: map[string]ServerEntry{}}
	if _, err := toml.DecodeFile(path, reg); err != nil {
		if os.IsNotExist(err) {
			return reg, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if reg.Servers == nil {
		reg.Servers = map[string]ServerEntry{}
	}
	return reg, nil
}

func (r *Registry) Save() error {
	path, err := registryPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := toml.NewEncoder(f).Encode(r); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// Lookup resolves an alias ("" means the default server).
func (r *Registry) Lookup(alias string) (string, ServerEntry, error) {
	if alias == "" {
		alias = r.Default
	}
	if alias == "" {
		return "", ServerEntry{}, fmt.Errorf("no server registered: run `crosstalk register <addr> <port> <alias> --token <TOKEN>` first")
	}
	e, ok := r.Servers[alias]
	if !ok {
		known := make([]string, 0, len(r.Servers))
		for a := range r.Servers {
			known = append(known, a)
		}
		sort.Strings(known)
		return "", ServerEntry{}, fmt.Errorf("no server registered under alias %q (known: %s)", alias, strings.Join(known, ", "))
	}
	return alias, e, nil
}

// SplitRoomRef resolves a room reference: `alias/room` when the prefix
// before the first slash is a registered alias, otherwise the whole string
// is a room on the default server. Room names may themselves contain
// slashes (owner/repo#7) as long as the first segment isn't an alias.
func (r *Registry) SplitRoomRef(ref string) (alias, room string, err error) {
	if i := strings.IndexByte(ref, '/'); i > 0 {
		if _, ok := r.Servers[ref[:i]]; ok {
			return ref[:i], ref[i+1:], nil
		}
	}
	if ref == "" {
		return "", "", fmt.Errorf("empty room reference")
	}
	alias = r.Default
	if alias == "" {
		return "", "", fmt.Errorf("no default server: register one or qualify the room as alias/room")
	}
	return alias, ref, nil
}

// Profile is profile.toml — the human display name for TUI mode. Agent mode
// ignores it entirely and requires --name explicitly.
type Profile struct {
	Name string `toml:"name"`
}

func LoadProfile() (*Profile, error) {
	dir, err := ConfigDir()
	if err != nil {
		return nil, err
	}
	p := &Profile{}
	if _, err := toml.DecodeFile(filepath.Join(dir, "profile.toml"), p); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return p, nil
}

func SaveProfile(p *Profile) error {
	dir, err := ConfigDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(dir, "profile.toml"))
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(p)
}
