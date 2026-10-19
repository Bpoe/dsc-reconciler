package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
)

type candidate struct {
	name    string // Slash-separated path relative to the configuration root.
	input   dsc.Input
	err     error
	changes inputChanges // Targeted hints must remain eligible after protection.
}

// inputChanges maps relative input names to whether an explicit removal was
// observed. Only explicit sidecar removal permits an absent-parameter attempt.
type inputChanges map[string]bool

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
	if !visiblePath(name) {
		return "", false
	}
	if parameterName(name) {
		return strings.TrimSuffix(configurationBase(name), ".parameters"), true
	}
	return configurationBase(name), configurationName(name)
}

func visiblePath(name string) bool {
	if !fs.ValidPath(name) || strings.ContainsAny(name, "\\\x00") || !filepath.IsLocal(filepath.FromSlash(name)) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}

func relativeName(root, file string) (string, error) {
	name, err := filepath.Rel(root, file)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(name), nil
}

func configurationLess(a, b string) bool {
	if path.Base(a) != path.Base(b) {
		return path.Base(a) < path.Base(b)
	}
	return a < b
}

func sortCandidates(items []candidate) {
	sort.Slice(items, func(i, j int) bool { return configurationLess(items[i].name, items[j].name) })
}

// changedCandidates uses ordinary discovery, including its ambiguity checks.
// Names are hints, not a cached list of inputs or permission to use deleted sidecars.
func changedCandidates(dir string, names inputChanges) ([]candidate, error) {
	directories := make(map[string]struct{})
	for name := range names {
		if _, ok := changedBase(name); ok {
			directories[path.Dir(name)] = struct{}{}
		}
	}
	var candidates []candidate
	var scanErr error
	for directory := range directories {
		if err := checkInputDirectory(dir, directory); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				scanErr = errors.Join(scanErr, err)
			}
			continue
		}
		items, err := discoverDirectory(dir, filepath.Join(dir, filepath.FromSlash(directory)))
		if !errors.Is(err, os.ErrNotExist) {
			scanErr = errors.Join(scanErr, err)
		}
		candidates = append(candidates, items...)
	}
	affected := make(map[string]bool)
	for _, item := range candidates {
		name := item.name
		base := configurationBase(name)
		if _, ok := names[name]; ok {
			affected[base] = true
		}
		for _, parameter := range parameterNames(base) {
			removed, ok := names[parameter]
			if !ok {
				continue
			}
			_, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(parameter)))
			if removed || !errors.Is(err, os.ErrNotExist) {
				// Unreadable/nonregular sidecars still need an input-failure result.
				affected[base] = true
			}
		}
	}
	selected := candidates[:0]
	for _, item := range candidates {
		if affected[configurationBase(item.name)] {
			item.changes = make(inputChanges)
			for name, removed := range names {
				if base, ok := changedBase(name); ok && base == configurationBase(item.name) {
					item.changes[name] = removed
				}
			}
			selected = append(selected, item)
		}
	}
	sortCandidates(selected)
	return selected, scanErr
}

func discover(dir string) ([]candidate, error) {
	return discoverTree(context.Background(), dir)
}

func discoverTree(ctx context.Context, dir string) ([]candidate, error) {
	var candidates []candidate
	err := walkDirectories(ctx, dir, func(directory string) error {
		items, err := discoverDirectory(dir, directory)
		candidates = append(candidates, items...)
		return err
	})
	sortCandidates(candidates)
	return candidates, err
}

// walkDirectories shares traversal rules with watch registration. Visit runs
// before enumerating children so a watch can cover arrivals during enumeration.
func walkDirectories(ctx context.Context, dir string, visit func(string) error) error {
	var scanErr error
	err := filepath.WalkDir(dir, func(file string, entry fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			scanErr = errors.Join(scanErr, fmt.Errorf("scan configuration tree %q: %w", file, err))
			return nil
		}
		if file != dir && strings.HasPrefix(entry.Name(), ".") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			if file == dir {
				return fmt.Errorf("configuration root %q is not a regular directory", dir)
			}
			return nil
		}
		if err := visit(file); err != nil {
			scanErr = errors.Join(scanErr, err)
			return filepath.SkipDir
		}
		return nil
	})
	return errors.Join(scanErr, err)
}

// Reject redirected ancestors as well as leaf symlinks. Producers are trusted;
// this check is not a transaction with subsequent opens.
func checkInputDirectory(root, directory string) error {
	if directory != "." && !visiblePath(directory) {
		return fmt.Errorf("invalid configuration directory %q", directory)
	}
	current := root
	parts := []string{"."}
	if directory != "." {
		parts = append(parts, strings.Split(directory, "/")...)
	}
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect configuration directory %q: %w", current, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("configuration directory %q is not a regular directory", current)
		}
	}
	return nil
}

func discoverDirectory(root, dir string) ([]candidate, error) {
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
		file := filepath.Join(dir, name)
		id, err := relativeName(root, file)
		if err != nil {
			return nil, err
		}
		item := candidate{name: id, input: dsc.Input{Configuration: file}}
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
