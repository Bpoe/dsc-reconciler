package reconcile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
)

type candidate struct {
	input dsc.Input
	err   error
}

func configurationName(name string) bool {
	if strings.HasPrefix(name, ".") || parameterName(name) {
		return false
	}
	ext := filepath.Ext(name)
	return ext == ".yaml" || ext == ".json"
}

func parameterName(name string) bool {
	return strings.HasSuffix(name, ".parameters.yaml") || strings.HasSuffix(name, ".parameters.json")
}

func configurationBase(name string) string {
	return strings.TrimSuffix(name, filepath.Ext(name))
}

func parameterNames(base string) []string {
	return []string{base + ".parameters.yaml", base + ".parameters.json"}
}

func changedBase(name string) (string, bool) {
	if strings.HasPrefix(name, ".") {
		return "", false
	}
	if parameterName(name) {
		return strings.TrimSuffix(configurationBase(name), ".parameters"), true
	}
	return configurationBase(name), configurationName(name)
}

// changedCandidates uses ordinary discovery, including its ambiguity checks.
// Names are hints, not a cached list of inputs or permission to use deleted sidecars.
func changedCandidates(dir string, names map[string]struct{}) ([]candidate, error) {
	candidates, err := discover(dir)
	if err != nil {
		return nil, err
	}
	affected := make(map[string]bool)
	for _, item := range candidates {
		name := filepath.Base(item.input.Configuration)
		base := configurationBase(name)
		if _, ok := names[name]; ok {
			affected[base] = true
		}
		for _, parameter := range parameterNames(base) {
			if _, ok := names[parameter]; !ok {
				continue
			}
			_, err := os.Lstat(filepath.Join(dir, parameter))
			if !errors.Is(err, os.ErrNotExist) {
				// Unreadable/nonregular sidecars still need an input-failure result.
				affected[base] = true
			}
		}
	}
	selected := candidates[:0]
	for _, item := range candidates {
		if affected[configurationBase(filepath.Base(item.input.Configuration))] {
			selected = append(selected, item)
		}
	}
	return selected, nil
}

func discover(dir string) ([]candidate, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("scan configuration directory %q: %w", dir, err)
	}
	present := make(map[string]bool, len(entries))
	byBase := make(map[string][]string)
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		present[name] = true
		if !configurationName(name) || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err == nil && !info.Mode().IsRegular() {
			continue
		}
		// Inaccessible candidates remain in the pass so they receive input-failure results.
		names = append(names, name)
		base := configurationBase(name)
		byBase[base] = append(byBase[base], name)
	}
	sort.Strings(names)
	candidates := make([]candidate, 0, len(names))
	for _, name := range names {
		base := configurationBase(name)
		item := candidate{input: dsc.Input{Configuration: filepath.Join(dir, name)}}
		if configurations := byBase[base]; len(configurations) > 1 {
			item.err = fmt.Errorf("configuration %q has ambiguous basename %q: multiple configuration files found: %s",
				name, base, strings.Join(configurations, ", "))
		}
		var parameters []string
		for _, parameter := range parameterNames(base) {
			if present[parameter] {
				parameters = append(parameters, parameter)
			}
		}
		switch len(parameters) {
		case 1:
			// Nonregular sidecars are errors on validation, not permission to use defaults.
			item.input.Parameters = filepath.Join(dir, parameters[0])
		case 2:
			item.err = errors.Join(item.err, fmt.Errorf("multiple parameter files found for configuration %q: %s",
				name, strings.Join(parameters, ", ")))
		}
		candidates = append(candidates, item)
	}
	return candidates, nil
}
