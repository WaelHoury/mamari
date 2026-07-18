package mamari

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Opt-in diagnostic test, skipped during normal test runs. It reproduces the
// long-session benchmark's edit-then-query cycle under deterministic synthetic
// CPU load (external `yes` processes, not just goroutines) to measure whether
// watch-mode query publishing closes the observed tail-latency gap.
func TestZZZContentionReproV2(t *testing.T) {
	if os.Getenv("MAMARI_CONTENTION_REPRO") != "1" {
		t.Skip("set MAMARI_CONTENTION_REPRO=1 to run the external CPU-load contention repro")
	}
	fixture := "/tmp/mamari-long-session-bench/mamari"
	if _, err := os.Stat(fixture + "/.mamari/index.json"); err != nil {
		t.Skipf("fixture not present: %v", err)
	}
	samples := 20
	if raw := os.Getenv("MAMARI_CONTENTION_SAMPLES"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			t.Fatalf("invalid MAMARI_CONTENTION_SAMPLES=%q", raw)
		}
		samples = n
	}
	runtime.GOMAXPROCS(runtime.NumCPU())

	var loaders []*exec.Cmd
	for i := 0; i < 4; i++ {
		cmd := exec.Command("yes")
		devnull, err := os.OpenFile("/dev/null", os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout = devnull
		if err := cmd.Start(); err != nil {
			_ = devnull.Close()
			t.Fatal(err)
		}
		_ = devnull.Close()
		loaders = append(loaders, cmd)
	}
	defer func() {
		for _, cmd := range loaders {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	time.Sleep(300 * time.Millisecond)

	idx, err := LoadIndex(fixture + "/.mamari/index.json")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = Watch(ctx, idx, WatchOptions{Debounce: 200 * time.Millisecond})
	}()
	time.Sleep(150 * time.Millisecond)

	editFiles := []string{
		"controllers/plaidController.js",
		"controllers/openBankingController.js",
		"services/userActivityService.js",
		"utilities/cronUtils.js",
		"routes/legacyRoutes.js",
		"controllers/blacklistController.js",
		"services/brokersUsersService.js",
		"middlewares/tokenMiddleWare.js",
		"controllers/internal/internalController.js",
		"services/auth/brokersAuthService.js",
	}
	query := "send email notification trigger which functions call sendEmail what triggers emails"
	originals := make(map[string][]byte, len(editFiles))
	for _, rel := range editFiles {
		path := fixture + "/" + rel
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		originals[rel] = stripContentionReproMarkers(data)
	}
	defer func() {
		for rel, data := range originals {
			_ = os.WriteFile(fixture+"/"+rel, data, 0o644)
		}
	}()

	t0 := time.Now()
	SearchCode(idx, query, SearchCodeOptions{Limit: 6, BudgetTokens: 1800})
	t.Logf("cold: %s", time.Since(t0))

	durations := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		rel := editFiles[i%len(editFiles)]
		path := fixture + "/" + rel
		data := append([]byte(nil), originals[rel]...)
		data = append(data, []byte(fmt.Sprintf("\n// contention-repro-v2-marker %d\n", i))...)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(900 * time.Millisecond)

		t0 := time.Now()
		SearchCode(idx, query, SearchCodeOptions{Limit: 6, BudgetTokens: 1800})
		dt := time.Since(t0)
		durations = append(durations, dt)
		t.Logf("cycle %2d (%s): %s", i, rel, dt)
	}
	t.Logf("summary: %s", durationSummary(durations))
}

func stripContentionReproMarkers(data []byte) []byte {
	var b strings.Builder
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if strings.Contains(line, "// contention-repro-v2-marker") {
			continue
		}
		b.WriteString(line)
	}
	return []byte(b.String())
}

func durationSummary(durations []time.Duration) string {
	if len(durations) == 0 {
		return "samples=0"
	}
	sorted := append([]time.Duration(nil), durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var total time.Duration
	for _, d := range sorted {
		total += d
	}
	pct := func(p float64) time.Duration {
		idx := int(float64(len(sorted))*p+0.999999) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		return sorted[idx]
	}
	return fmt.Sprintf("samples=%d mean=%s p50=%s p90=%s p95=%s max=%s",
		len(sorted),
		total/time.Duration(len(sorted)),
		pct(0.50),
		pct(0.90),
		pct(0.95),
		sorted[len(sorted)-1],
	)
}
