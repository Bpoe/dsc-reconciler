package reconcile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
	"go.yaml.in/yaml/v3"
)

func configurationOperation(path, text string) (dsc.Operation, error) {
	// JSON is also allowed in .yaml files. Use its own decoder: YAML parsers
	// need not accept every JSON string escape (notably UTF-16 surrogate pairs).
	raw := []byte(text)
	if json.Valid(raw) {
		return jsonConfigurationOperation(path, raw)
	}
	if filepath.Ext(path) == ".json" {
		return "", fmt.Errorf("configuration %q must be valid JSON", path)
	}
	decoder := yaml.NewDecoder(strings.NewReader(text))
	var document, trailing yaml.Node
	if decoder.Decode(&document) != nil || decoder.Decode(&trailing) != io.EOF {
		// Parser errors can include input values, so do not expose their text.
		return "", fmt.Errorf("configuration %q must contain a single valid JSON or YAML document", path)
	}
	node := &document
	location := "configuration"
	for _, key := range []string{"metadata", "dscd", "operation"} {
		// Decode only this mapping level; unrelated values remain untyped nodes.
		// Decode also resolves YAML mapping aliases and merge keys.
		var fields map[string]yaml.Node
		if node.Decode(&fields) != nil || fields == nil {
			return "", fmt.Errorf("configuration %q: %s must be an object with unique keys", path, location)
		}
		value, ok := fields[key]
		if !ok {
			return dsc.OperationSet, nil
		}
		node = &value
		location += "." + key
	}
	for node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	operation := dsc.Operation(node.Value)
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" ||
		(operation != dsc.OperationSet && operation != dsc.OperationTest) {
		return "", fmt.Errorf("configuration %q: metadata.dscd.operation must be \"set\" or \"test\"", path)
	}
	return operation, nil
}

func jsonConfigurationOperation(path string, raw []byte) (dsc.Operation, error) {
	// The entire document has passed json.Valid, including values we do not inspect.
	location := "configuration"
	for _, key := range []string{"metadata", "dscd", "operation"} {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		token, err := decoder.Token()
		if err != nil || token != json.Delim('{') {
			return "", fmt.Errorf("configuration %q: %s must be an object", path, location)
		}
		var selected json.RawMessage
		seen := make(map[string]bool)
		for decoder.More() {
			token, err := decoder.Token()
			name, ok := token.(string)
			if err != nil || !ok || seen[name] {
				return "", fmt.Errorf("configuration %q: %s must have unique keys", path, location)
			}
			seen[name] = true
			var value json.RawMessage
			if decoder.Decode(&value) != nil {
				return "", fmt.Errorf("configuration %q must be valid JSON", path)
			}
			if name == key {
				selected = value
			}
		}
		if selected == nil {
			return dsc.OperationSet, nil
		}
		raw = selected
		location += "." + key
	}
	var operation dsc.Operation
	if json.Unmarshal(raw, &operation) != nil ||
		(operation != dsc.OperationSet && operation != dsc.OperationTest) {
		return "", fmt.Errorf("configuration %q: metadata.dscd.operation must be \"set\" or \"test\"", path)
	}
	return operation, nil
}
