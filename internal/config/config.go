// Package config parses and validates daemon options.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Options configures one daemon instance.
type Options struct {
	Version          bool
	ConfigDir        string
	ResultsDir       string
	DSCPath          string
	Interval         time.Duration
	ExecutionTimeout time.Duration
}

// Parse parses flags without changing the filesystem.
func Parse(args []string, output io.Writer) (Options, error) {
	input, results := defaultDirectories()
	o := Options{}
	f := flag.NewFlagSet("dscd", flag.ContinueOnError)
	f.SetOutput(output)
	f.BoolVar(&o.Version, "version", false, "print dscd version and exit")
	f.StringVar(&o.ConfigDir, "config-dir", input, "directory of DSC documents")
	f.StringVar(&o.ResultsDir, "results-dir", results, "directory for latest results")
	f.StringVar(&o.DSCPath, "dsc-path", "dsc", "DSC executable name or path")
	f.DurationVar(&o.Interval, "interval", 5*time.Minute, "delay after each completed reconciliation pass (positive)")
	f.DurationVar(&o.ExecutionTimeout, "execution-timeout", 15*time.Minute, "maximum DSC request duration per document (positive)")
	if err := f.Parse(args); err != nil {
		return o, err
	}
	if f.NArg() != 0 {
		return o, errors.New("positional arguments are not supported")
	}
	if o.Interval <= 0 || o.ExecutionTimeout <= 0 {
		return o, errors.New("interval and execution-timeout must be positive")
	}
	if o.ConfigDir == "" || o.ResultsDir == "" || o.DSCPath == "" {
		return o, errors.New("config-dir, results-dir and dsc-path must not be empty")
	}
	return o, nil
}

// Prepare resolves paths and checks inputs. Result-store preparation belongs to results.
func (o *Options) Prepare() error {
	var err error
	o.ConfigDir, err = canonicalPath(o.ConfigDir)
	if err != nil {
		return fmt.Errorf("resolve configuration directory: %w", err)
	}
	if _, err = os.ReadDir(o.ConfigDir); err != nil {
		return fmt.Errorf("read configuration directory %q: %w", o.ConfigDir, err)
	}
	o.ResultsDir, err = canonicalPath(o.ResultsDir)
	if err != nil {
		return fmt.Errorf("resolve results directory: %w", err)
	}
	if within(o.ConfigDir, o.ResultsDir) || within(o.ResultsDir, o.ConfigDir) {
		return errors.New("configuration and results directories must be separate, non-nested directories")
	}
	o.DSCPath, err = exec.LookPath(o.DSCPath)
	if err != nil {
		return fmt.Errorf("resolve DSC executable: %w", err)
	}
	o.DSCPath, err = filepath.Abs(o.DSCPath)
	if err != nil {
		return fmt.Errorf("resolve absolute DSC path: %w", err)
	}
	info, err := os.Stat(o.DSCPath)
	if err != nil {
		return fmt.Errorf("stat DSC executable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("DSC executable must be a regular file")
	}
	return nil
}

func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Resolve the existing prefix too, so aliases of not-yet-created outputs are rejected.
func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) || filepath.Dir(abs) == abs {
		return "", err
	}
	parent, err := canonicalPath(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}
