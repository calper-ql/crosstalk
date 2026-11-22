package client

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/calper-ql/crosstalk/internal/proto"
)

// What the CLI and the TUI share about files: reading one to send, and
// putting a fetched one on disk.

// ReadFileToSend reads the file at path, after asking the server how large a
// file it takes: a refusal that can be known beforehand is given here,
// before megabytes are read and sent. It returns the name to send it under.
func ReadFileToSend(c *Conn, path string) (name string, data []byte, err error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", nil, err
	}
	if !st.Mode().IsRegular() {
		return "", nil, fmt.Errorf("%s is not a regular file", path)
	}
	if st.Size() == 0 {
		return "", nil, fmt.Errorf("%s is empty", path)
	}
	ver, err := c.Version()
	if err != nil {
		return "", nil, err
	}
	if ver.MaxFile == 0 {
		return "", nil, errors.New("this server takes no files (it is older than file sharing, or has it switched off)")
	}
	if st.Size() > ver.MaxFile {
		return "", nil, fmt.Errorf("%s is %s; this server takes files up to %s",
			path, proto.HumanSize(st.Size()), proto.HumanSize(ver.MaxFile))
	}
	if data, err = os.ReadFile(path); err != nil {
		return "", nil, err
	}
	return filepath.Base(path), data, nil
}

// FileErr rewords the one failure of a file call that the server cannot
// explain itself: a server from before files has no such route at all.
func FileErr(err error) error {
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == 404 && ae.Code == "http_error" {
		return errors.New("this server takes no files: it is older than file sharing")
	}
	return err
}

// fetchLimit bounds what one fetch reads. Far above any cap a server sets:
// it is a guard against a stream that never ends, not a policy.
const fetchLimit = 1 << 30

// Fetch gets a file a message carried, with its name cleaned for use in a
// path (SafeFileName).
func (c *Conn) Fetch(room, id string) (proto.File, []byte, error) {
	meta, data, err := c.File(room, id, fetchLimit)
	if err != nil {
		return proto.File{}, nil, FileErr(err)
	}
	meta.Name = SafeFileName(meta.Name)
	return meta, data, nil
}

// keepFetched is how long a fetched file stays in ~/.crosstalk/files before
// a later fetch clears it away. Rooms and their files are short-lived; what
// was copied out of them should not pile up for ever either.
const keepFetched = 7 * 24 * time.Hour

// SaveFetched writes a fetched file under ~/.crosstalk/files and returns its
// path. The id leads the name, so two files of one name never collide, and a
// name chosen by a stranger can never be one that means something by itself
// (.bashrc, Makefile): it is always "<id>-<name>". And the name is spelled
// with nothing a shell reads as anything but a name (DiskName): the path is
// printed for an agent whose next step is a command with it in.
func SaveFetched(meta proto.File, data []byte) (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "files")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	clearOldFetched(dir)
	path := filepath.Join(dir, DiskName(meta.ID)+"-"+DiskName(meta.Name))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// SaveFetchedAs writes a fetched file to a path the caller chose, and only
// if nothing is there: the sender's name for the file plays no part in it,
// and nothing is overwritten.
func SaveFetchedAs(path string, data []byte) error {
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		return fmt.Errorf("%s is a directory: give the path of the file to write (the sender's name for it is not used)", path)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s is already there: nothing was written — choose another path", path)
		}
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

// SafeFileName is the client's own cleaning of a file's name before it goes
// anywhere near a path or a terminal: the last path element, no control or
// formatting characters, bounded. The server cleans names too; a client does
// not take that on trust.
func SafeFileName(name string) string {
	name = strings.ToValidUTF8(name, "")
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if len(name) > 200 {
		name = strings.ToValidUTF8(name[len(name)-200:], "") // the end holds the extension
	}
	if name == "" || name == "." || name == ".." {
		return "file"
	}
	return name
}

// DiskName spells a file's name for a path that will be printed and typed:
// letters, digits, dot, underscore and hyphen only, every run of anything
// else as one underscore. So no space to split it, and no $, backtick, quote,
// semicolon or bracket for a shell to act on, whatever the sender called the
// file. What the file was called is still reported, as data, beside the path.
func DiskName(name string) string {
	var b strings.Builder
	gap := false
	for _, r := range SafeFileName(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-' {
			if gap && b.Len() > 0 {
				b.WriteByte('_')
			}
			gap = false
			b.WriteRune(r)
		} else {
			gap = true
		}
	}
	if s := strings.Trim(b.String(), "."); s != "" {
		return b.String()
	}
	return "file"
}

// clearOldFetched removes what earlier fetches left in dir more than
// keepFetched ago. Only plain files directly in it, and failures are not
// worth reporting: the next fetch tries again.
func clearOldFetched(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err == nil && info.Mode().IsRegular() && time.Since(info.ModTime()) > keepFetched {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
