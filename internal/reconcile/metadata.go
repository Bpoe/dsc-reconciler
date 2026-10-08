package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
)

type metadata struct {
	Operation json.RawMessage `json:"operation"`
}

func readMetadata(ctx context.Context, path string) (dsc.Operation, error) {
	text, err := readInputFile(ctx, path, maxInputBytes)
	if err != nil {
		return "", fmt.Errorf("read metadata: %w", err)
	}
	var meta metadata
	if !strings.HasPrefix(strings.TrimSpace(text), "{") || json.Unmarshal([]byte(text), &meta) != nil {
		return "", fmt.Errorf("metadata %q must be a valid JSON object", path)
	}
	if meta.Operation == nil {
		return dsc.OperationSet, nil
	}
	var operation dsc.Operation
	if json.Unmarshal(meta.Operation, &operation) != nil ||
		(operation != dsc.OperationSet && operation != dsc.OperationTest) {
		return "", fmt.Errorf("metadata %q operation must be \"set\" or \"test\"", path)
	}
	return operation, nil
}
