package files_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dashpay/dash-network-go/internal/files"
)

func TestArtifactPublicationIsPrivateAndNonOverwriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock.json")
	if err := files.WriteJSON(path, map[string]int{"generation": 1}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("file mode %o", info.Mode().Perm())
	}
	if err := files.WriteJSON(path, map[string]int{"generation": 2}); err == nil {
		t.Fatal("overwrote existing evidence")
	}
	var got struct {
		Generation int `json:"generation"`
	}
	if err := files.ReadJSON(path, &got); err != nil || got.Generation != 1 {
		t.Fatal("original evidence changed")
	}
}

func TestReadRejectsTrailingOrUnknownJSON(t *testing.T) {
	for _, data := range []string{`{"generation":1} {"generation":2}`, `{"generation":1,"secret":"not-an-allowed-field"}`} {
		p := filepath.Join(t.TempDir(), "input.json")
		if err := os.WriteFile(p, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		var value struct {
			Generation int `json:"generation"`
		}
		if err := files.ReadJSON(p, &value); err == nil {
			t.Fatal("ambiguous JSON accepted")
		}
	}
}

func TestTextTrustPublicationIsPrivateAndNonOverwriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := files.WriteText(path, "original\n"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("trust permissions", err)
	}
	if err = files.WriteText(path, "replacement\n"); err == nil {
		t.Fatal("trust overwritten")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "original\n" {
		t.Fatal("trust changed", err)
	}
}
