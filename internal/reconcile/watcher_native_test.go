package reconcile

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
)

func TestNativeWatcherPublication(t *testing.T) {
	for _, kind := range []string{"configuration create", "configuration write", "configuration rename", "configuration replace",
		"parameters create", "parameters write", "parameters rename", "parameters replace", "parameter first",
		"parameters delete", "parameters move away", "parameters hide"} {
		t.Run(kind, func(t *testing.T) {
			for _, location := range []string{"root", "nested"} {
				t.Run(location, func(t *testing.T) { testNativeWatcherPublication(t, kind, location) })
			}
		})
	}
}

func testNativeWatcherPublication(t *testing.T, kind, location string) {
	root := t.TempDir()
	input(t, root, "unrelated.yaml")
	dir := root
	if location == "nested" {
		dir = filepath.Join(root, "managed", "deep")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	config := "web.yaml"
	parameters := "web.parameters.json"
	configurationExists := kind != "configuration create" && kind != "configuration rename" && kind != "parameter first"
	if configurationExists {
		input(t, dir, config)
	}
	if kind == "parameters write" || kind == "parameters replace" || kind == "parameters delete" ||
		kind == "parameters move away" || kind == "parameters hide" {
		writeInput(t, dir, parameters, `{"message":"old"}`)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	attempts := make(chan dsc.Input, 128)
	closed := make(chan struct{}, 128)
	var logs bytes.Buffer
	writer := &fakeWriter{}
	r := New(root, time.Hour, func(context.Context) (DSC, error) {
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
	}, writer, slog.New(slog.NewTextHandler(&logs, nil)))
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
	case "parameters delete":
		if err := os.Remove(filepath.Join(dir, parameters)); err != nil {
			t.Fatal(err)
		}
	case "parameters move away", "parameters hide":
		target := filepath.Join(t.TempDir(), parameters)
		if kind == "parameters hide" {
			target = filepath.Join(dir, "."+parameters)
		}
		if err := os.Rename(filepath.Join(dir, parameters), target); err != nil {
			t.Fatal(err)
		}
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
			<-done
			for _, result := range writer.results {
				if result.Error != nil {
					t.Logf("input failure: %s", result.Error.Message)
				}
			}
			t.Fatalf("did not reconcile published inputs: %v\n%s\nresults: %+v", ctx.Err(), logs.String(), writer.results)
		}
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

func TestNativeRecursiveDirectoryLifecycle(t *testing.T) {
	root := t.TempDir()
	input(t, root, "00-anchor.yaml")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	attempts := make(chan dsc.Input, 128)
	registered := make(chan string, 128)
	r := New(root, time.Hour, fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
		select {
		case attempts <- in:
		case <-ctx.Done():
		}
		return dsc.Result{Outcome: "succeeded"}
	}}.start, &fakeWriter{}, logger())
	r.watch = func() (eventSource, error) {
		source, err := newEventSource()
		if err != nil {
			return source, err
		}
		add := source.add
		source.add = func(dir string) error {
			if err := add(dir); err != nil {
				return err
			}
			name, err := relativeName(root, dir)
			select {
			case registered <- name:
			case <-ctx.Done():
				return ctx.Err()
			}
			return err
		}
		return source, nil
	}
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("native recursive watcher did not stop")
		}
	}()
	waitFor := func(name, text string) {
		t.Helper()
		for {
			select {
			case in := <-attempts:
				id, _ := relativeName(root, in.Configuration)
				if id == name && in.ConfigurationText == text {
					return
				}
				if id == "00-anchor.yaml" {
					t.Fatal("directory event executed an unrelated configuration")
				}
			case <-ctx.Done():
				t.Fatalf("no native attempt for %s: %v", name, ctx.Err())
			}
		}
	}
	waitFor("00-anchor.yaml", `{"resources":[]}`)
	newDir := filepath.Join(root, "new", "deep")
	if err := os.MkdirAll(newDir, 0700); err != nil {
		t.Fatal(err)
	}
awaitingWatch:
	for {
		select {
		case name := <-registered:
			if name == "new/deep" {
				break awaitingWatch
			}
		case <-ctx.Done():
			t.Fatal("new directory was not watched")
		}
	}
	publishInput(t, newDir, "app.yaml", `{"resources":[],"version":1}`)
	waitFor("new/deep/app.yaml", `{"resources":[],"version":1}`)

	input(t, root, ".staging/deep/staged.yaml")
	if err := os.Rename(filepath.Join(root, ".staging"), filepath.Join(root, "managed")); err != nil {
		t.Fatal(err)
	}
	waitFor("managed/deep/staged.yaml", `{"resources":[]}`)
	publishInput(t, filepath.Join(root, "managed", "deep"), "staged.yaml", `{"resources":[],"version":2}`)
	waitFor("managed/deep/staged.yaml", `{"resources":[],"version":2}`)
	if err := os.Rename(filepath.Join(root, "managed"), filepath.Join(root, ".retired")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, ".retired"), filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	waitFor("moved/deep/staged.yaml", `{"resources":[],"version":2}`)
	publishInput(t, filepath.Join(root, "moved", "deep"), "staged.yaml", `{"resources":[],"version":3}`)
	waitFor("moved/deep/staged.yaml", `{"resources":[],"version":3}`)
}

func TestNativeRetiresRenamedSubtree(t *testing.T) {
	dir := t.TempDir()
	input(t, dir, "nested/deep/web.yaml")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pending := newPendingChanges()
	w, err := startDirectoryWatch(ctx, dir, newEventSource, pending)
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	if err := os.Rename(filepath.Join(dir, "nested"), filepath.Join(dir, ".retired")); err != nil {
		t.Fatal(err)
	}
	for {
		if err := w.update(ctx, dir, pending); err != nil {
			t.Fatal(err)
		}
		if len(w.directories) == 1 {
			return
		}
		select {
		case <-pending.wake:
		case <-w.failed:
			t.Fatalf("watcher failed: %v", w.failure())
		case <-ctx.Done():
			t.Fatal("renamed subtree retained obsolete watches")
		}
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
		case <-current.failed:
			t.Fatalf("replacement watch failed: %v", current.failure())
		case <-ctx.Done():
			t.Fatal("replacement watch did not receive published configuration")
		}
	}
}
