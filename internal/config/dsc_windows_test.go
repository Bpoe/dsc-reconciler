package config

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPrepareBundledDSC(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(filepath.Dir(executable), "dsc")
	if err := os.Mkdir(bundle, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bundle) })
	bundled := filepath.Join(bundle, "dsc.exe")
	if err := os.WriteFile(bundled, []byte("bundled; never invoked"), 0700); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	bin := filepath.Join(root, "external")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(bin, "dsc.exe")
	if err := os.WriteFile(external, []byte("external; never invoked"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("PATHEXT", ".EXE")
	// The working directory has no relationship to the installed executable.
	t.Chdir(root)
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"automatic bundle", nil, bundled},
		{"explicit PATH name", []string{"-dsc-path", "dsc"}, external},
		{"explicit path", []string{"-dsc-path", executable}, executable},
		{"invalid override", []string{"-dsc-path", filepath.Join(root, "missing.exe")}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"-config-dir", bin, "-results-dir", filepath.Join(root, "results")}, test.args...)
			o, err := Parse(args, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			err = o.Prepare()
			if test.want == "" {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("invalid override: %v", err)
				}
				return
			}
			if err != nil || o.DSCPath != test.want {
				t.Fatalf("selected %q, %v; want %q", o.DSCPath, err, test.want)
			}
		})
	}
	if err := os.Remove(bundled); err != nil {
		t.Fatal(err)
	}
	o, err := Parse([]string{"-config-dir", bin, "-results-dir", filepath.Join(root, "results")}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Prepare(); err != nil || o.DSCPath != external {
		t.Fatalf("PATH fallback: %q, %v", o.DSCPath, err)
	}
	t.Setenv("PATH", "")
	o.DSCPath = "dsc"
	if err := o.Prepare(); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("missing bundle and PATH: %v", err)
	}
}
