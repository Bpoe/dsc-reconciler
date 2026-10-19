package reconcile

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Bpoe/dsc-reconciler/internal/dsc"
	"golang.org/x/sys/unix"
)

func TestConfigurationLockProbe(t *testing.T) {
	file := os.Getenv("DSCD_LOCK_PROBE")
	if file == "" {
		return
	}
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if os.Getenv("DSCD_LOCK_EXPECTED") == "held" {
		if !errors.Is(err, unix.EWOULDBLOCK) {
			t.Fatalf("conflicting lock = %v, want EWOULDBLOCK", err)
		}
	} else if err != nil {
		t.Fatalf("lock leaked: %v", err)
	}
}

func probeConfigurationLock(t *testing.T, file string, held bool) {
	t.Helper()
	state := "released"
	if held {
		state = "held"
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestConfigurationLockProbe$")
	cmd.Env = append(os.Environ(), "DSCD_LOCK_PROBE="+file, "DSCD_LOCK_EXPECTED="+state)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("external lock probe (%s): %v\n%s", state, err, output)
	}
}

func TestLinuxProtectionThroughPublication(t *testing.T) {
	for _, failure := range []string{"none", "execution", "publication", "input", "startup", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			file := writeInput(t, dir, "web.yaml", `{"resources":[]}`)
			if failure == "input" {
				writeInput(t, dir, "web.yaml", "{")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			publishing, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
				probeConfigurationLock(t, file, true)
				if failure == "canceled" {
					cancel()
				}
				if failure == "execution" {
					return dsc.InputFailure(in, errors.New("execution failed"))
				}
				return dsc.Result{Outcome: "succeeded"}
			}}
			start := func(ctx context.Context) (DSC, error) {
				probeConfigurationLock(t, file, true)
				if failure == "startup" {
					return nil, errors.New("start failed")
				}
				return client, nil
			}
			writer := &fakeWriter{write: func(context.Context, dsc.Result) error {
				close(publishing)
				<-release
				if failure == "publication" {
					return errors.New("publication failed")
				}
				return nil
			}}
			done := make(chan error, 1)
			go func() { done <- New(dir, time.Second, start, writer, logger()).Pass(ctx) }()
			select {
			case <-publishing:
			case <-time.After(10 * time.Second):
				t.Fatal("publication did not start")
			}
			probeConfigurationLock(t, file, true)
			close(release)
			select {
			case err := <-done:
				if err != nil && failure != "startup" {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("reconciliation did not finish")
			}
			probeConfigurationLock(t, file, false)
		})
	}
}

func TestLinuxStaleConfigurationWhileWaiting(t *testing.T) {
	for _, change := range []string{"replace", "remove"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			file := writeInput(t, dir, "web.yaml", `{"resources":[]}`)
			held, err := openConfiguration(t.Context(), file)
			if err != nil {
				t.Fatal(err)
			}
			defer held.Close()
			writer := &fakeWriter{}
			calls := 0
			client := fakeDSC{run: func(_ context.Context, in dsc.Input) dsc.Result {
				calls++
				return dsc.Result{Outcome: "succeeded"}
			}}
			r := New(dir, time.Second, client.start, writer, logger())
			synctest.Test(t, func(t *testing.T) {
				done := make(chan error, 1)
				go func() { done <- r.Pass(t.Context()) }()
				synctest.Wait() // The descriptor is open and waiting on the held flock.
				if change == "replace" {
					replacement := writeInput(t, dir, ".replacement", `{"resources":[],"new":true}`)
					if err := os.Rename(replacement, file); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				if err := held.Close(); err != nil {
					t.Fatal(err)
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			})
			if calls != 0 || len(writer.snapshot()) != 0 {
				t.Fatal("stale configuration executed or published a result")
			}
			if change == "replace" {
				if err := r.Pass(t.Context()); err != nil || calls != 1 {
					t.Fatalf("replacement was not reconciled on next pass: %v, calls=%d", err, calls)
				}
			}
		})
	}
}

func TestLinuxCancelLockWait(t *testing.T) {
	dir := t.TempDir()
	file := writeInput(t, dir, "web.yaml", `{"resources":[]}`)
	held, err := openConfiguration(t.Context(), file)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	descriptorCount := func() int {
		t.Helper()
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, entry := range entries {
			target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
			if err == nil && target == file {
				count++
			}
		}
		return count
	}
	before := descriptorCount()
	writer := &fakeWriter{}
	client := fakeDSC{run: func(context.Context, dsc.Input) dsc.Result {
		t.Error("executed while another owner held the lock")
		return dsc.Result{}
	}}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- New(dir, time.Second, client.start, writer, logger()).Pass(ctx) }()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("lock wait cancellation = %v", err)
		}
	})
	if len(writer.snapshot()) != 0 {
		t.Fatal("canceled lock wait published a result")
	}
	if got := descriptorCount(); got != before {
		t.Fatalf("canceled lock wait leaked a descriptor: before=%d, after=%d", before, got)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	probeConfigurationLock(t, filepath.Join(dir, "web.yaml"), false)
}
