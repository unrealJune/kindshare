package main

// Persistent device identity.
//
// Everything the phone uses to recognise us was regenerated per process before
// this: the endpoint id, the mDNS host label derived from it, and the 16-byte
// metadata blob in the TXT record. A daemon that restarts - a reboot, a wifi
// toggle, `kindshare-svc.sh restart` - therefore came back as a stranger, while
// the phone's old entry stayed in the share sheet pointing at an endpoint id
// nothing answers to any more. The visible symptom is two identically named
// Kindles in the sheet, one of them dead, with no way to tell which.
//
// So identity is generated exactly once and lives in a file. The format is
// key=value lines because a KOReader Lua plugin and a busybox shell both have
// to read and write it without a parser.

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// defaultIdentityPath is on /mnt/us so it survives a firmware update, next to
// the autostart flag and the binary.
const defaultIdentityPath = "/mnt/us/kindler/identity"

// identity is the stable half of what we advertise. Name is the display name
// without the suffix; ID is both the Quick Share endpoint id and the visible
// suffix; DevID is the salt + encrypted metadata key.
type identity struct {
	Name  string
	ID    string
	DevID []byte

	path      string
	ephemeral bool // not backed by a file; regenerated next start
}

// Display is what the phone shows. The id is always appended: two devices
// running this binary are otherwise indistinguishable in the share sheet, and
// that ambiguity is the whole reason this file exists. Five characters is a
// cheap price for knowing which tile is which.
func (id *identity) Display() string {
	if id.ID == "" {
		return id.Name
	}
	return id.Name + " " + id.ID
}

// newDevID is the 16-byte metadata blob: salt + encrypted metadata key.
func newDevID() []byte {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// loadIdentity reads the identity file, generating and persisting any field
// that is missing or malformed. A file that cannot be written is not fatal -
// we fall back to an ephemeral identity and say so, because refusing to
// advertise at all would be a worse failure than advertising unstably.
func loadIdentity(path string) *identity {
	if path == "" {
		path = defaultIdentityPath
	}
	id := &identity{path: path, Name: defaultDeviceName}

	changed := false
	kv, err := readKV(path)
	if err != nil && !os.IsNotExist(err) {
		log.Printf("identity: cannot read %s: %v", path, err)
	}

	if v := strings.TrimSpace(kv["name"]); v != "" {
		id.Name = v
	} else {
		changed = true
	}

	if v := strings.TrimSpace(kv["id"]); isValidID(v) {
		id.ID = v
	} else {
		if v != "" {
			log.Printf("identity: ignoring malformed id %q, generating a new one", v)
		}
		id.ID = string(endpointID())
		changed = true
	}

	if v, err := hex.DecodeString(strings.TrimSpace(kv["devid"])); err == nil && len(v) == 16 {
		id.DevID = v
	} else {
		id.DevID = newDevID()
		changed = true
	}

	if changed {
		if err := id.save(); err != nil {
			id.ephemeral = true
			log.Printf("identity: NOT PERSISTED (%v) - the phone will see a new device "+
				"after every restart; pass -identity to a writable path", err)
		}
	}
	return id
}

// isValidID matches what endpointID generates: exactly 4 alphanumerics. The
// value ends up in a DNS label and in a fixed-width slot in the service
// instance name, so a hand-edited file cannot be allowed to widen it.
func isValidID(s string) bool {
	if len(s) != 4 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// save writes the file atomically so the plugin never reads a half-written one.
func (id *identity) save() error {
	if err := os.MkdirAll(filepath.Dir(id.path), 0o755); err != nil {
		return err
	}
	body := fmt.Sprintf(""+
		"# kindshare device identity - generated once, then reused.\n"+
		"# name  what the phone shows, minus the suffix\n"+
		"# id    4 alphanumerics: Quick Share endpoint id AND the visible suffix\n"+
		"# devid 16 bytes hex: salt + encrypted metadata key\n"+
		"# Change name or id here or in the KOReader plugin, then restart the\n"+
		"# service. Changing id makes the phone treat this as a new device.\n"+
		"name=%s\nid=%s\ndevid=%s\n",
		id.Name, id.ID, hex.EncodeToString(id.DevID))

	tmp := id.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, id.path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// readKV parses the key=value lines, ignoring blanks and # comments.
func readKV(path string) (map[string]string, error) {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	return out, sc.Err()
}
