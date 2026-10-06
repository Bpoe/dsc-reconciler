package reconcile

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"

	"dsc-reconciler/internal/dsc"
)

func inputHash(ctx context.Context, input dsc.Input) (string, error) {
	configuration, err := fileHash(ctx, input.Configuration)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte("dscd-input-v1\x00"))
	h.Write(configuration[:])
	if input.Parameters == "" {
		h.Write([]byte{0})
	} else {
		parameters, err := fileHash(ctx, input.Parameters)
		if err != nil {
			return "", err
		}
		h.Write([]byte{1})
		h.Write(parameters[:])
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil)), nil
}

func fileHash(ctx context.Context, path string) (sum [sha256.Size]byte, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("read reconciliation input %q: %w", path, err)
		}
	}()
	if err := ctx.Err(); err != nil {
		return sum, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return sum, err
	}
	if !info.Mode().IsRegular() {
		return sum, errors.New("not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return sum, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err = f.Stat()
	if err != nil {
		return sum, err
	}
	if !info.Mode().IsRegular() {
		return sum, errors.New("opened input is not a regular file")
	}
	h := sha256.New()
	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		n, err := f.Read(buffer)
		h.Write(buffer[:n])
		if errors.Is(err, io.EOF) {
			copy(sum[:], h.Sum(nil))
			return sum, nil
		}
		if err != nil {
			return sum, err
		}
	}
}
