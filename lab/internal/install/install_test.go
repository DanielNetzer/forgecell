package install

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T, version string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "forgecell")
	if err := os.WriteFile(file, []byte("#!/bin/sh\nprintf '%s\\n' '"+version+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return file
}
func TestUpgradeRollbackPreserveLab(t *testing.T) {
	home := filepath.Join(t.TempDir(), "with spaces'", "lab")
	bin := filepath.Join(t.TempDir(), "bin")
	os.MkdirAll(filepath.Join(home, "ledgers"), 0700)
	os.WriteFile(filepath.Join(home, "ledgers", "keep"), []byte("ledger"), 0600)
	o := Options{Home: home, Bin: bin, Source: fixture(t, "0.2.0"), Version: "0.2.0"}
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	o.Source = fixture(t, "0.2.1")
	o.Version = "0.2.1"
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if _, err := Rollback(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(home, "current"))
	if err != nil || filepath.Base(target) != "0.2.0" {
		t.Fatalf("%s %v", target, err)
	}
	raw, err := os.ReadFile(filepath.Join(home, "ledgers", "keep"))
	if err != nil || string(raw) != "ledger" {
		t.Fatal("Lab data changed")
	}
}
func TestBrokenReleaseDoesNotReplaceCurrent(t *testing.T) {
	o := Options{Home: t.TempDir(), Bin: t.TempDir(), Source: fixture(t, "0.2.0"), Version: "0.2.0"}
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	o.Source = fixture(t, "wrong")
	o.Version = "0.2.1"
	if _, err := Install(context.Background(), o); err == nil {
		t.Fatal("bad smoke accepted")
	}
	target, _ := os.Readlink(filepath.Join(o.Home, "current"))
	if filepath.Base(target) != "0.2.0" {
		t.Fatal("current changed")
	}
}
func TestRefuseUnmanagedCommandAndCorruptRollback(t *testing.T) {
	o := Options{Home: t.TempDir(), Bin: t.TempDir(), Source: fixture(t, "0.2.0"), Version: "0.2.0"}
	os.WriteFile(filepath.Join(o.Bin, "forgecell"), []byte("mine"), 0600)
	if _, err := Install(context.Background(), o); err == nil {
		t.Fatal("overwrote unrelated command")
	}
	os.Remove(filepath.Join(o.Bin, "forgecell"))
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	o.Source = fixture(t, "0.2.1")
	o.Version = "0.2.1"
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(o.Home, "releases", "0.2.0", "forgecell"), []byte("corrupt"), 0700)
	if _, err := Rollback(context.Background(), o); err == nil {
		t.Fatal("corrupt rollback accepted")
	}
	target, _ := os.Readlink(filepath.Join(o.Home, "current"))
	if filepath.Base(target) != "0.2.1" {
		t.Fatal("current changed")
	}
}

func TestExposedCommandMayBeManagedLauncher(t *testing.T) {
	home := t.TempDir()
	o := Options{Home: home, Bin: filepath.Join(home, "bin"), Source: fixture(t, "0.2.0"), Version: "0.2.0"}
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(filepath.Join(o.Bin, "forgecell"))
	if err != nil || !st.Mode().IsRegular() {
		t.Fatalf("launcher replaced by symlink: %v", err)
	}
}

func TestSymlinkAliasOfLauncherDirectory(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, "bin"), 0700)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(filepath.Join(home, "bin"), alias); err != nil {
		t.Fatal(err)
	}
	o := Options{Home: home, Bin: alias, Source: fixture(t, "0.2.0"), Version: "0.2.0"}
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(filepath.Join(home, "bin", "forgecell"))
	if err != nil || !st.Mode().IsRegular() {
		t.Fatalf("aliased launcher is not regular: %v", err)
	}
}
