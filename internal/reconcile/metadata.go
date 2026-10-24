package reconcile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
	"go.yaml.in/yaml/v3"
)

type metadata struct {
	operation dsc.Operation
	frequency time.Duration
}

func configurationMetadata(path, text string) (metadata, error) {
	// JSON is also allowed in .yaml files. Use its own decoder: YAML parsers
	// need not accept every JSON string escape (notably UTF-16 surrogate pairs).
	raw := []byte(text)
	if json.Valid(raw) {
		return jsonConfigurationMetadata(path, raw)
	}
	if filepath.Ext(path) == ".json" {
		return metadata{}, fmt.Errorf("configuration %q must be valid JSON", path)
	}
	decoder := yaml.NewDecoder(strings.NewReader(text))
	var document, trailing yaml.Node
	if decoder.Decode(&document) != nil || decoder.Decode(&trailing) != io.EOF {
		// Parser errors can include input values, so do not expose their text.
		return metadata{}, fmt.Errorf("configuration %q must contain a single valid JSON or YAML document", path)
	}
	node := &document
	location := "configuration"
	var fields map[string]yaml.Node
	for _, key := range []string{"", "metadata", "dscd"} {
		if key != "" {
			value, ok := fields[key]
			if !ok {
				return defaultMetadata(dsc.OperationSet), nil
			}
			node = &value
			location += "." + key
		}
		// Decode only this mapping level; unrelated values remain untyped nodes.
		// Decode also resolves YAML mapping aliases and merge keys.
		fields = nil
		if node.Decode(&fields) != nil || fields == nil {
			return metadata{}, fmt.Errorf("configuration %q: %s must be an object with unique keys", path, location)
		}
	}
	return parseMetadata(path, func(name string) (string, bool, bool) {
		value, present := fields[name]
		for value.Kind == yaml.AliasNode {
			value = *value.Alias
		}
		return value.Value, present, value.Kind == yaml.ScalarNode && value.Tag == "!!str"
	})
}

func jsonConfigurationMetadata(path string, raw []byte) (metadata, error) {
	// The entire document has passed json.Valid, including values we do not inspect.
	location := "configuration"
	var fields map[string]json.RawMessage
	for _, key := range []string{"", "metadata", "dscd"} {
		if key != "" {
			selected := fields[key]
			if selected == nil {
				return defaultMetadata(dsc.OperationSet), nil
			}
			raw = selected
			location += "." + key
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		token, err := decoder.Token()
		if err != nil || token != json.Delim('{') {
			return metadata{}, fmt.Errorf("configuration %q: %s must be an object", path, location)
		}
		fields = make(map[string]json.RawMessage)
		for decoder.More() {
			token, err := decoder.Token()
			name, ok := token.(string)
			if err != nil || !ok || fields[name] != nil {
				return metadata{}, fmt.Errorf("configuration %q: %s must have unique keys", path, location)
			}
			var value json.RawMessage
			if decoder.Decode(&value) != nil {
				return metadata{}, fmt.Errorf("configuration %q must be valid JSON", path)
			}
			fields[name] = value
		}
	}
	return parseMetadata(path, func(name string) (string, bool, bool) {
		raw, present := fields[name]
		var value string
		err := json.Unmarshal(raw, &value)
		return value, present, err == nil && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
	})
}

func defaultMetadata(operation dsc.Operation) metadata {
	frequency := 15 * time.Minute
	if operation == dsc.OperationExport {
		frequency = 24 * time.Hour
	}
	return metadata{operation: operation, frequency: frequency}
}

func parseMetadata(path string, field func(string) (value string, present, isString bool)) (metadata, error) {
	operation := dsc.OperationSet
	if value, present, isString := field("operation"); present {
		operation = dsc.Operation(value)
		if !isString || !operation.Valid() {
			return metadata{}, fmt.Errorf("configuration %q: metadata.dscd.operation must be \"set\", \"test\" or \"export\"", path)
		}
	}
	options := defaultMetadata(operation)
	if value, present, isString := field("frequency"); present {
		frequency, err := time.ParseDuration(value)
		if !isString || err != nil || frequency <= 0 {
			return metadata{}, fmt.Errorf("configuration %q: metadata.dscd.frequency must be a positive Go duration string", path)
		}
		options.frequency = frequency
	}
	return options, nil
}
