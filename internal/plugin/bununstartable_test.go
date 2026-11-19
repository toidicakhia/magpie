package plugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func stubReported(t *testing.T, f func(string) string) {
	t.Helper()
	old := bunReported
	bunReported = f
	t.Cleanup(func() { bunReported = old })
}

// A Bun that this Windows can't load isn't a wall: a compatible build the
// reader already has, installed as `bun`, is taken with no setting, copied
// in where magpie keeps its Buns, and used from then on.
func TestUnstartableBunTakesOneOnPath(t *testing.T) {
	bunHome(t, BunVersion, time.Now())
	onPath := filepath.Join(t.TempDir(), bunExe())
	if err := os.WriteFile(onPath, []byte("a win7 bun"), 0o755); err != nil {
		t.Fatal(err)
	}
	pathBunsAre(t, onPath)
	stubReported(t, func(string) string { return BunVersion })
	stubTry(t, func(string, string) error { return nil })

	err := fmt.Errorf("starting plugins: %w", &BunUnstartableError{Bun: `C:\cache\bun\magpie-bun.exe`, Why: "windows", Err: exitedWith(statusEntrypointNotFound)})
	got, ok := bunOnUnstartable(context.Background(), err)
	if !ok || got != bunExeOf(BunVersion) {
		t.Fatalf("bunOnUnstartable = %q, %v", got, ok)
	}
	if b, _ := os.ReadFile(got); string(b) != "a win7 bun" {
		t.Fatalf("the bun on PATH wasn't copied to %s: %q", got, b)
	}
}

// Only a Windows this machine can't load the Bun on switches it. A CPU
// failure, an ordinary error and a Bun the reader named keep their own
// paths: those already say what to do and mustn't be replaced silently.
func TestUnstartableBunLeavesOtherFailuresAlone(t *testing.T) {
	bunHome(t, BunVersion, time.Now())
	onPath := filepath.Join(t.TempDir(), bunExe())
	if err := os.WriteFile(onPath, []byte("a win7 bun"), 0o755); err != nil {
		t.Fatal(err)
	}
	pathBunsAre(t, onPath)
	stubReported(t, func(string) string { return BunVersion })
	stubTry(t, func(string, string) error { return nil })
	ctx := context.Background()

	for _, err := range []error{
		&BunUnstartableError{Bun: "bun", Why: "cpu", Err: exitedWith(statusIllegalInstruction)},
		fmt.Errorf("bun add x: exit status 1"),
	} {
		if got, ok := bunOnUnstartable(ctx, err); ok || got != "" {
			t.Errorf("%v: switched to %q", err, got)
		}
	}
	t.Setenv("MAGPIE_BUN", `D:\bun.exe`)
	err := &BunUnstartableError{Bun: "bun", Why: "windows", Err: exitedWith(statusEntrypointNotFound)}
	if got, ok := bunOnUnstartable(ctx, err); ok || got != "" {
		t.Errorf("MAGPIE_BUN set: switched to %q", got)
	}
}
