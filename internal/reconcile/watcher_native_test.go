package reconcile

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
)

func TestNativeWatcherPublication(t *testing.T) {
	for _, kind := range []string{"configuration create", "configuration write", "configuration rename", "configuration replace",
		"parameters create", "parameters write", "parameters rename", "parameters replace", "parameter first"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			input(t, dir, "unrelated.yaml")
			config := "web.yaml"
			parameters := "web.parameters.json"
			configurationExists := kind != "configuration create" && kind != "configuration rename" && kind != "parameter first"
			if configurationExists {
				input(t, dir, config)
			}
			if kind == "parameters write" || kind == "parameters replace" {
				writeInput(t, dir, parameters, `{"message":"old"}`)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			attempts := make(chan dsc.Input, 128)
			closed := make(chan struct{}, 128)
			r := New(dir, time.Hour, func(context.Context) (DSC, error) {
				return &fakeSession{
					execute: func(_ context.Context, in dsc.Input) (dsc.Result, error) {
						select {
						case attempts <- in:
						case <-ctx.Done():
							return dsc.InputFailure(in, ctx.Err()), ctx.Err()
						}
						return dsc.Result{Configuration: filepath.Base(in.Configuration), Operation: &in.Operation, Outcome: "succeeded"}, nil
					},
					close: func() error {
						select {
						case closed <- struct{}{}:
						case <-ctx.Done():
						}
						return nil
					},
				}, nil
			}, &fakeWriter{}, logger())
			done := make(chan struct{})
			go func() { r.Run(ctx); close(done) }()
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("native watcher failed to shut down")
				}
			}()
			initial := 1
			if configurationExists {
				initial++
			}
			for range initial {
				select {
				case <-attempts:
				case <-ctx.Done():
					t.Fatal("startup pass did not execute")
				}
			}
			select {
			case <-closed:
			case <-ctx.Done():
				t.Fatal("startup pass did not close its session")
			}
			const newConfig = `{"metadata":{"dscd":{"operation":"test"}},"resources":[]}`
			const newParameters = `{"message":"ready"}`
			expectedConfig := `{"resources":[]}`
			expectedParameters := ""
			switch kind {
			case "configuration create", "configuration write":
				writeInput(t, dir, config, newConfig)
				expectedConfig = newConfig
			case "configuration rename", "configuration replace":
				publishInput(t, dir, config, newConfig)
				expectedConfig = newConfig
			case "parameters create", "parameters write":
				writeInput(t, dir, parameters, newParameters)
				expectedParameters = newParameters
			case "parameters rename", "parameters replace":
				publishInput(t, dir, parameters, newParameters)
				expectedParameters = newParameters
			case "parameter first":
				publishInput(t, dir, parameters, newParameters)
				publishInput(t, dir, config, newConfig)
				expectedConfig, expectedParameters = newConfig, newParameters
			}
			for {
				select {
				case in := <-attempts:
					if filepath.Base(in.Configuration) != config {
						t.Fatalf("filesystem change executed unrelated input: %+v", in)
					}
					if in.ConfigurationText == expectedConfig && in.ParametersText == expectedParameters {
						if expectedParameters != "" && filepath.Base(in.Parameters) != parameters {
							t.Fatal("lost parameter association")
						}
						return
					}
				case <-ctx.Done():
					t.Fatalf("did not reconcile published inputs: %v", ctx.Err())
				}
			}
		})
	}
}

func publishInput(t *testing.T, dir, name, text string) {
	t.Helper()
	temp, err := os.CreateTemp(dir, ".publish-*")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := fmt.Fprint(temp, text); err != nil {
		temp.Close()
		t.Fatal(err)
	}
	if err := temp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := renameInput(temp.Name(), filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}

func TestNativeDirectoryWatchReplacement(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "inputs")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pending := newPendingChanges()
	old, err := startDirectoryWatch(ctx, dir, newEventSource, pending)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, filepath.Join(root, "old")); err != nil {
		old.close()
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		old.close()
		t.Fatal(err)
	}
	identityErr := old.validate(dir)
	if err := old.close(); err != nil {
		t.Fatal(err)
	}
	if identityErr == nil {
		t.Fatal("native directory replacement retained old identity")
	}
	current, err := startDirectoryWatch(ctx, dir, newEventSource, pending)
	if err != nil {
		t.Fatal(err)
	}
	defer current.close()
	publishInput(t, dir, "web.yaml", `{"resources":[]}`)
	for {
		if _, ok := pending.take()["web"]; ok {
			return
		}
		select {
		case <-pending.wake:
		case <-current.done:
			t.Fatalf("replacement watch failed: %v", current.failure())
		case <-ctx.Done():
			t.Fatal("replacement watch did not receive published configuration")
		}
	}
}
