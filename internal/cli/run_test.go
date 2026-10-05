package cli

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// passLog records the passes a loop runs and fails the test on any overlap.
type passLog struct {
	t        *testing.T
	inFlight atomic.Int32
	count    atomic.Int32
	hold     time.Duration
	mu       sync.Mutex
	triggers []string
}

func (p *passLog) pass(context.Context) int {
	if p.inFlight.Add(1) > 1 {
		p.t.Error("two passes ran at once")
	}
	time.Sleep(p.hold)
	// In this order, so a test that sees the count move knows the pass is over.
	p.inFlight.Add(-1)
	p.count.Add(1)
	return ExitOK
}

func (p *passLog) allTriggers() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.triggers)
}

func (p *passLog) started(trigger string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.triggers = append(p.triggers, trigger)
}

func (p *passLog) lastTrigger() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.triggers[len(p.triggers)-1]
}

// startLoop runs a loop until the test ends, with the trigger channel shaped
// exactly as serve builds it.
func startLoop(t *testing.T, interval, debounce time.Duration, p *passLog) func() {
	t.Helper()
	trigger := make(chan struct{}, 1)
	l := loop{interval: interval, debounce: debounce, trigger: trigger, run: p.pass, started: p.started}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := l.until(ctx); err != nil {
			t.Errorf("loop: %v", err)
		}
	}()
	t.Cleanup(func() { cancel(); <-done })

	waitFor(t, "the startup pass", func() bool { return p.count.Load() == 1 })
	return func() {
		select {
		case trigger <- struct{}{}:
		default:
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSchedulerCoalescesABurst(t *testing.T) {
	p := &passLog{t: t}
	fire := startLoop(t, time.Hour, 200*time.Millisecond, p)

	// Spread across the debounce rather than sent at once, so each event
	// reaches the loop on its own instead of collapsing in the channel.
	for range 10 {
		fire()
		time.Sleep(10 * time.Millisecond)
	}
	if n := p.count.Load(); n != 1 {
		t.Errorf("%d passes ran 100ms into a 200ms debounce, want none yet", n-1)
	}
	waitFor(t, "the webhook pass", func() bool { return p.count.Load() == 2 })
	time.Sleep(300 * time.Millisecond)
	if n := p.count.Load(); n != 2 {
		t.Errorf("a burst of 10 events ran %d passes after startup, want 1", n-1)
	}
	if got := p.lastTrigger(); got != "webhook" {
		t.Errorf("the pass was attributed to %q, want webhook", got)
	}
}

func TestSchedulerFollowsAPassThatWasInFlight(t *testing.T) {
	p := &passLog{t: t, hold: 150 * time.Millisecond}
	fire := startLoop(t, time.Hour, 20*time.Millisecond, p)

	fire()
	waitFor(t, "the first webhook pass to start", func() bool { return p.inFlight.Load() == 1 })
	// Mid-pass: these must produce exactly one more pass, not zero, not three.
	fire()
	fire()
	fire()
	waitFor(t, "the follow-up pass", func() bool { return p.count.Load() == 3 })
	time.Sleep(300 * time.Millisecond)
	if n := p.count.Load(); n != 3 {
		t.Errorf("events during a pass gave %d follow-ups, want 1", n-2)
	}
}

func TestSchedulerSteadyStreamDoesNotStarve(t *testing.T) {
	p := &passLog{t: t}
	fire := startLoop(t, time.Hour, 60*time.Millisecond, p)

	// An event every 10ms, well inside the debounce. A sliding window would
	// keep pushing the pass back and never run it.
	stop := time.After(400 * time.Millisecond)
	for tick := time.Tick(10 * time.Millisecond); ; {
		select {
		case <-tick:
			fire()
			continue
		case <-stop:
		}
		break
	}
	// At most one pass per debounce, plus slack for scheduling: without the
	// debounce this would be one pass per event, about forty.
	if n := p.count.Load() - 1; n < 2 || n > 400/60+2 {
		t.Errorf("400ms of steady events ran %d passes, want between 2 and %d", n, 400/60+2)
	}
}

func TestSchedulerAttributesEachPass(t *testing.T) {
	p := &passLog{t: t}
	fire := startLoop(t, 150*time.Millisecond, 20*time.Millisecond, p)

	fire()
	waitFor(t, "the interval pass after the webhook one", func() bool { return p.count.Load() >= 3 })
	if got := p.allTriggers()[:3]; !slices.Equal(got, []string{"timer", "webhook", "timer"}) {
		t.Errorf("passes were attributed %v, want [timer webhook timer]", got)
	}
}

func TestSchedulerTimerStillFiresWithoutEvents(t *testing.T) {
	p := &passLog{t: t}
	startLoop(t, 40*time.Millisecond, 10*time.Millisecond, p)

	waitFor(t, "three timer passes", func() bool { return p.count.Load() >= 4 })
	if got := p.lastTrigger(); got != "timer" {
		t.Errorf("an interval pass was attributed to %q, want timer", got)
	}
}

func TestSchedulerNeverDelaysANearerTimer(t *testing.T) {
	p := &passLog{t: t}
	// The interval is due before the debounce would be: an event must not
	// push the pass out to the debounce.
	fire := startLoop(t, 80*time.Millisecond, time.Hour, p)

	fire()
	waitFor(t, "the interval pass", func() bool { return p.count.Load() == 2 })
	if got := p.lastTrigger(); got != "timer" {
		t.Errorf("the pass was attributed to %q, want timer", got)
	}
}

// signedDaemonConfig is writeConfig with a webhook secret on the cloud side
// and both listeners on free loopback ports.
func signedDaemonConfig(t *testing.T) (cfgPath, metricsAddr, webhookAddr string) {
	t.Helper()
	fake := writableFake()
	cfgPath = writeConfig(t, fake.start(t), filepath.Join(t.TempDir(), "state.json"))
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	signed := strings.Replace(string(raw), "        tokenEnv: CLOUD_TOKEN\n",
		"        tokenEnv: CLOUD_TOKEN\n    webhookSecretEnv: CLOUD_WEBHOOK_SECRET\n", 1)
	metricsAddr, webhookAddr = freeAddr(t), freeAddr(t)
	signed += "\ndaemon:\n  interval: 1h\nmetrics:\n  listen: \"" + metricsAddr +
		"\"\nwebhook:\n  listen: \"" + webhookAddr + "\"\n  debounce: 1s\n"
	if err := os.WriteFile(cfgPath, []byte(signed), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, metricsAddr, webhookAddr
}

func TestDaemonRunsAPassOnASignedWebhook(t *testing.T) {
	t.Setenv("CLOUD_TOKEN", "token-a")
	t.Setenv("SELF_TOKEN", "token-b")
	t.Setenv("CLOUD_WEBHOOK_SECRET", "e2e-secret")
	cfgPath, metricsAddr, webhookAddr := signedDaemonConfig(t)

	ctx, cancel := context.WithCancel(t.Context())
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- Run(ctx, []string{"plan", "--config", cfgPath, "--daemon"}, &stdout, &stderr) }()
	defer func() { cancel(); <-done }()
	pollMetrics(t, "http://"+metricsAddr+"/metrics")
	// The two listeners come up independently; metrics answering says
	// nothing about the webhook port.
	waitFor(t, "the webhook listener", func() bool {
		c, err := net.Dial("tcp", webhookAddr)
		if err == nil {
			_ = c.Close()
		}
		return err == nil
	})

	ts := time.Now().UnixMilli()
	body := `{"event":"secrets.modified","project":{"projectId":"p","environment":"prod","secretPath":"/arr-stack"},"timestamp":` +
		strconv.FormatInt(ts, 10) + `}`
	mac := hmac.New(sha256.New, []byte("e2e-secret"))
	mac.Write([]byte(body))
	req, _ := http.NewRequest(http.MethodPost, "http://"+webhookAddr+"/webhook/cloud", strings.NewReader(body))
	req.Header.Set("X-Infisical-Signature", "t="+strconv.FormatInt(ts, 10)+";"+hex.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("posting the webhook: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("webhook status = %d, want 202\n%s", resp.StatusCode, stderr.String())
	}

	want := `infisical_mirror_passes_started_total{trigger="webhook"} 1`
	waitFor(t, "a webhook-triggered pass", func() bool {
		return strings.Contains(getBody(t, "http://"+metricsAddr+"/metrics"), want)
	})

	// Each port serves only its own routes.
	for url, code := range map[string]int{
		"http://" + webhookAddr + "/metrics":       http.StatusNotFound,
		"http://" + metricsAddr + "/webhook/cloud": http.StatusNotFound,
	} {
		r, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
		if r.StatusCode != code {
			t.Errorf("GET %s = %d, want %d", url, r.StatusCode, code)
		}
	}
}

func TestDaemonRefusesAnEmptyWebhookSecret(t *testing.T) {
	t.Setenv("CLOUD_TOKEN", "token-a")
	t.Setenv("SELF_TOKEN", "token-b")
	t.Setenv("CLOUD_WEBHOOK_SECRET", " ")
	cfgPath, _, _ := signedDaemonConfig(t)

	code, _, stderr := run(t, "plan", "--config", cfgPath, "--daemon")
	if code != ExitError || !strings.Contains(stderr, "CLOUD_WEBHOOK_SECRET is unset or empty") {
		t.Errorf("exit %d, stderr %q; want a refusal naming the variable", code, stderr)
	}
}

func getBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
