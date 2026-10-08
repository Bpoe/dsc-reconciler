package config

import (
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	o, err := Parse(nil, io.Discard)
	if err != nil || o.Version || o.Interval != 5*time.Minute || o.ExecutionTimeout != 15*time.Minute || o.DSCPath != "dsc" {
		t.Fatalf("defaults: %+v, %v", o, err)
	}
	input, output := defaultDirectories()
	if o.ConfigDir != input || o.ResultsDir != output {
		t.Fatalf("platform defaults: %+v", o)
	}
	for _, args := range [][]string{
		{"-interval", "0"}, {"-interval", "-1s"}, {"-interval", "bad"},
		{"-execution-timeout", "0"}, {"-config-dir", ""}, {"-results-dir", ""},
		{"-dsc-path", ""}, {"unexpected"}, {"-unknown"},
		{"--version=invalid"}, {"--version", "unexpected"}, {"--version", "--unknown"},
	} {
		if _, err := Parse(args, io.Discard); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestParseVersion(t *testing.T) {
	for _, test := range []struct {
		arg  string
		want bool
	}{
		{"--version", true}, {"-version", true},
		{"--version=true", true}, {"--version=false", false},
	} {
		t.Run(test.arg, func(t *testing.T) {
			o, err := Parse([]string{test.arg}, io.Discard)
			if err != nil || o.Version != test.want {
				t.Fatalf("version: %+v, %v", o, err)
			}
		})
	}
	var help strings.Builder
	if _, err := Parse([]string{"--help"}, &help); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help: %v", err)
	}
	if !strings.Contains(help.String(), "-version") {
		t.Fatalf("version missing from help: %s", help.String())
	}
}

func TestIntervalOverride(t *testing.T) {
	o, err := Parse([]string{"-interval", "30s"}, io.Discard)
	if err != nil || o.Interval != 30*time.Second {
		t.Fatalf("explicit interval: %+v, %v", o, err)
	}
}

func TestPrepare(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "input with spaces")
	if err := os.Mkdir(input, 0700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, in, out, executable string
		valid                     bool
	}{
		{"valid", input, filepath.Join(root, "new results"), executable, true},
		{"same", input, input, executable, false},
		{"nested output", input, filepath.Join(input, "results"), executable, false},
		{"nested input", input, root, executable, false},
		{"missing input", filepath.Join(root, "missing"), filepath.Join(root, "results"), executable, false},
		{"missing executable", input, filepath.Join(root, "results"), filepath.Join(root, "missing-dsc"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := Options{ConfigDir: test.in, ResultsDir: test.out, DSCPath: test.executable}
			err := o.Prepare()
			if (err == nil) != test.valid {
				t.Fatalf("Prepare = %v", err)
			}
			if test.valid && (!filepath.IsAbs(o.ConfigDir) || !filepath.IsAbs(o.ResultsDir) || !filepath.IsAbs(o.DSCPath)) {
				t.Fatalf("non-absolute paths: %+v", o)
			}
		})
	}
	if runtime.GOOS == "windows" {
		o := Options{ConfigDir: input, ResultsDir: filepath.Join(root, "INPUT WITH SPACES"), DSCPath: executable}
		if o.Prepare() == nil {
			t.Fatal("accepted case-insensitive alias")
		}
	}
}

func TestPrepareSymlinkAlias(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "input")
	alias := filepath.Join(root, "alias")
	if err := os.Mkdir(input, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(input, alias); err != nil {
		t.Skipf("symlink privilege unavailable: %v", err)
	}
	executable, _ := os.Executable()
	o := Options{ConfigDir: input, ResultsDir: filepath.Join(alias, "not-yet-created"), DSCPath: executable}
	if o.Prepare() == nil {
		t.Fatal("accepted nested output through alias")
	}
}

func TestPrepareDefaultDSCFromPATH(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "input")
	bin := filepath.Join(root, "DSC with spaces")
	for _, dir := range []string{input, bin} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	name := "dsc"
	if runtime.GOOS == "windows" {
		name += ".exe"
		t.Setenv("PATHEXT", ".EXE")
	}
	options := func() Options {
		o, err := Parse([]string{"-config-dir", input, "-results-dir", filepath.Join(root, "results")}, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	o := options()
	if err := o.Prepare(); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("missing DSC on PATH: %v", err)
	}
	executable := filepath.Join(bin, name)
	if err := os.WriteFile(executable, []byte("test executable; never invoked"), 0700); err != nil {
		t.Fatal(err)
	}
	o = options()
	if err := o.Prepare(); err != nil {
		t.Fatal(err)
	}
	if o.DSCPath != executable {
		t.Fatalf("DSCPath = %q, want %q", o.DSCPath, executable)
	}
}
