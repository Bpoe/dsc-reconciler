package reconcile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"dsc-reconciler/internal/dsc"
)

type candidate struct {
	input dsc.Input
	err   error
}

func configurationName(name string) bool {
	if strings.HasPrefix(name, ".") ||
		strings.HasSuffix(name, ".parameters.yaml") || strings.HasSuffix(name, ".parameters.json") {
		return false
	}
	ext := filepath.Ext(name)
	return ext == ".yaml" || ext == ".json"
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
		base := strings.TrimSuffix(name, filepath.Ext(name))
		byBase[base] = append(byBase[base], name)
	}
	sort.Strings(names)
	candidates := make([]candidate, 0, len(names))
	for _, name := range names {
		base := strings.TrimSuffix(name, filepath.Ext(name))
		item := candidate{input: dsc.Input{Configuration: filepath.Join(dir, name)}}
		if configurations := byBase[base]; len(configurations) > 1 {
			item.err = fmt.Errorf("configuration %q has ambiguous basename %q: multiple configuration files found: %s",
				name, base, strings.Join(configurations, ", "))
		}
		var parameters []string
		for _, suffix := range []string{".parameters.yaml", ".parameters.json"} {
			if present[base+suffix] {
				parameters = append(parameters, base+suffix)
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
