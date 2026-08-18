package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The whole point of the file is that a second load returns the first load's
// values. If this breaks, the phone starts seeing a new device on every restart
// and the stale entries come back.
func TestIdentityIsStableAcrossLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity")

	first := loadIdentity(path)
	if first.ephemeral {
		t.Fatalf("identity not persisted to a writable temp dir")
	}
	if !isValidID(first.ID) {
		t.Fatalf("generated id %q is not 4 alphanumerics", first.ID)
	}
	if len(first.DevID) != 16 {
		t.Fatalf("devid is %d bytes, want 16", len(first.DevID))
	}

	second := loadIdentity(path)
	if second.ID != first.ID {
		t.Errorf("id changed across loads: %q then %q", first.ID, second.ID)
	}
	if second.Name != first.Name {
		t.Errorf("name changed across loads: %q then %q", first.Name, second.Name)
	}
	if string(second.DevID) != string(first.DevID) {
		t.Errorf("devid changed across loads")
	}
}

func TestIdentityDisplayAppendsID(t *testing.T) {
	id := &identity{Name: "Kindle Voyage", ID: "m4fx"}
	if got, want := id.Display(), "Kindle Voyage m4fx"; got != want {
		t.Errorf("Display() = %q, want %q", got, want)
	}

	// hostLabel takes the base name and appends the id itself. Feeding it
	// Display() would produce kindle-voyage-m4fx-m4fx.
	if got, want := hostLabel(id.Name, id.ID), "kindle-voyage-m4fx"; got != want {
		t.Errorf("hostLabel(Name) = %q, want %q", got, want)
	}
}

func TestIdentityHonoursEditedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(path, []byte(
		"# a comment\n\nname = Junes Kindle \nid=Ab12\ndevid="+
			strings.Repeat("ab", 16)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	id := loadIdentity(path)
	if id.Name != "Junes Kindle" {
		t.Errorf("name = %q, want %q", id.Name, "Junes Kindle")
	}
	if id.ID != "Ab12" {
		t.Errorf("id = %q, want %q", id.ID, "Ab12")
	}
	if len(id.DevID) != 16 || id.DevID[0] != 0xab {
		t.Errorf("devid not read back from file: %x", id.DevID)
	}
}

// A hand-edited id of the wrong shape would corrupt the DNS label and the
// fixed-width slot in the service instance name, so it is replaced rather than
// trusted.
func TestIdentityRejectsMalformedID(t *testing.T) {
	for _, bad := range []string{"toolong", "ab", "ab-c", ""} {
		path := filepath.Join(t.TempDir(), "identity")
		if err := os.WriteFile(path, []byte("name=X\nid="+bad+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		id := loadIdentity(path)
		if !isValidID(id.ID) {
			t.Errorf("id %q accepted from file, want regenerated", bad)
		}
		// And the correction must be written back, not re-rolled next time.
		if again := loadIdentity(path); again.ID != id.ID {
			t.Errorf("regenerated id for %q was not persisted", bad)
		}
	}
}

// An unwritable path must not be fatal: advertising with an unstable identity
// still beats not advertising.
func TestIdentityFallsBackToEphemeral(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	id := loadIdentity(filepath.Join(blocker, "identity"))
	if !id.ephemeral {
		t.Errorf("expected ephemeral fallback when the path is unwritable")
	}
	if !isValidID(id.ID) {
		t.Errorf("ephemeral identity still needs a usable id, got %q", id.ID)
	}
}
