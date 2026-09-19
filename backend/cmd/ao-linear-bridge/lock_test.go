package main

import (
	"path/filepath"
	"testing"
)

func TestDatabaseLockExcludesSecondProcessAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.lock")
	release, err := lock(path)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := lock(path); err == nil {
		other()
		release()
		t.Fatal("second lock succeeded")
	}
	release()
	again, err := lock(path)
	if err != nil {
		t.Fatal(err)
	}
	again()
}
