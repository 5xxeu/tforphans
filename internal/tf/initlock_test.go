package tf

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestMain lets this test binary be terragrunt's terraform, as tforphans is.
func TestMain(m *testing.M) {
	RunWrapper()
	os.Exit(m.Run())
}

// fakeTerraform writes a terraform whose init fails when another init
// runs at the same time, and whose state pull prints {}.
func fakeTerraform(t *testing.T, dir string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts as terraform and terragrunt")
	}
	busy := filepath.Join(dir, "busy")
	bin := filepath.Join(dir, "terraform")
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
init) mkdir '%s' || exit 3; sleep 0.2; rmdir '%s' ;;
state) printf '{}' ;;
plan) exit 7 ;;
esac
`, busy, busy)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// TestInitTurns: terragrunt's own inits (units that have a
// .terragrunt-cache too: an init cut short leaves one) and terraform init
// of roots never run at the same time.
func TestInitTurns(t *testing.T) {
	dir := t.TempDir()
	tfBin := fakeTerraform(t, dir)
	tg := filepath.Join(dir, "terragrunt")
	if err := os.WriteFile(tg, []byte("#!/bin/sh\n\"$TG_TF_PATH\" init -input=false && exec \"$TG_TF_PATH\" state pull\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 5)
	for i := range errs {
		unit := filepath.Join(dir, fmt.Sprintf("unit%d", i))
		if err := os.MkdirAll(filepath.Join(unit, ".terragrunt-cache"), 0o755); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 1 {
				errs[i] = Runner{Bin: tfBin, Dir: unit}.Init(context.Background())
				return
			}
			out, err := TerragruntStatePull(context.Background(), tg, tfBin, unit, 0)
			if err == nil && string(out) != "{}" {
				err = fmt.Errorf("state %q", out)
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("%d: %v", i, err)
		}
	}
}

func TestWrapper(t *testing.T) {
	tfBin := fakeTerraform(t, t.TempDir())
	if code := runWrapper(tfBin, []string{"plan"}); code != 7 {
		t.Errorf("exit code of terraform: %d", code)
	}
	if got := subcommand([]string{"-chdir=x", "init", "-input=false"}); got != "init" {
		t.Errorf("subcommand after a global option: %q", got)
	}
	// No turn, no init.
	t.Setenv(wrapEnvSocket, "")
	if code := runWrapper(tfBin, []string{"init"}); code != 1 {
		t.Errorf("init without a turn: %d", code)
	}
}

// TestBudget: waiting for the init turn does not use up -source-timeout,
// and cancel still ends the wait.
func TestBudget(t *testing.T) {
	initTurn <- struct{}{}
	go func() {
		time.Sleep(400 * time.Millisecond)
		<-initTurn
	}()
	ctx, cancel := WithBudget(context.Background(), 200*time.Millisecond)
	defer cancel()
	unlock, err := lockInit(ctx)
	if err != nil {
		t.Fatalf("budget ran out while waiting: %v", err)
	}
	unlock()
	time.Sleep(50 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatalf("budget gone right after the wait: %v", ctx.Err())
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("budget never ran out")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Errorf("error of a spent budget: %v", ctx.Err())
	}
	// Children see the same error.
	child, stop := context.WithCancel(ctx)
	defer stop()
	if !errors.Is(child.Err(), context.DeadlineExceeded) {
		t.Errorf("child of a spent budget: %v", child.Err())
	}

	initTurn <- struct{}{}
	defer func() { <-initTurn }()
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel2()
	}()
	if _, err := lockInit(ctx2); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled wait: %v", err)
	}
}
