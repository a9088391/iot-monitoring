package main

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"
)

func TestDemoPasswordsSurviveBootstrapRetry(t *testing.T) {
	directory := t.TempDir()
	state, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	if _, err := demoPasswords(state, false); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("existing database accepted a missing credential file")
	}

	first, err := demoPasswords(state, true)
	if err != nil {
		t.Fatal(err)
	}
	hq, north, south := first["hq"], first["north"], first["south"]
	if len(first) != 3 || !validSecret(hq) || !validSecret(north) || !validSecret(south) ||
		hq == north || hq == south || north == south {
		t.Fatal("expected distinct random credentials for the three demo accounts")
	}
	info, err := state.Stat(demoPasswordsName)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode() != 0o600 {
		t.Fatalf("got mode %v, want regular file with 0600", info.Mode())
	}
	for _, initialize := range []bool{true, false} {
		reused, err := demoPasswords(state, initialize)
		if err != nil {
			t.Fatal(err)
		}
		if !maps.Equal(reused, first) {
			t.Fatal("bootstrap replaced previously published credentials")
		}
	}
	if err := os.Chmod(filepath.Join(directory, demoPasswordsName), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := demoPasswords(state, true); err == nil {
		t.Fatal("bootstrap accepted credentials accessible by other users")
	}
}
