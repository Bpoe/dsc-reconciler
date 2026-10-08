package reconcile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"unicode/utf8"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
)

const maxInputBytes = 16 << 20

func readInput(ctx context.Context, input dsc.Input) (dsc.Input, error) {
	var err error
	input.ConfigurationText, err = readInputFile(ctx, input.Configuration, maxInputBytes)
	if err == nil && input.Parameters != "" {
		input.ParametersText, err = readInputFile(ctx, input.Parameters, maxInputBytes-len(input.ConfigurationText))
	}
	return input, err
}

func inputHash(input dsc.Input) string {
	configuration := sha256.Sum256([]byte(input.ConfigurationText))
	h := sha256.New()
	h.Write([]byte("dscd-input-v2\x00"))
	h.Write(configuration[:])
	if input.Parameters == "" {
		h.Write([]byte{0})
	} else {
		parameters := sha256.Sum256([]byte(input.ParametersText))
		h.Write([]byte{1})
		h.Write(parameters[:])
	}
	h.Write([]byte{0})
	h.Write([]byte(input.Operation))
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
}

func readInputFile(ctx context.Context, path string, limit int) (text string, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("read reconciliation input %q: %w", path, err)
		}
	}()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err = f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("opened input is not a regular file")
	}
	var content bytes.Buffer
	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := f.Read(buffer)
		if content.Len()+n > limit {
			return "", fmt.Errorf("input exceeds the allowed byte limit (%d bytes)", limit)
		}
		content.Write(buffer[:n])
		if errors.Is(err, io.EOF) {
			if !utf8.Valid(content.Bytes()) {
				return "", errors.New("input must be UTF-8 for lossless JSON-RPC submission")
			}
			return content.String(), nil
		}
		if err != nil {
			return "", err
		}
	}
}
