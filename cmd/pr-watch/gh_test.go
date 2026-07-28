package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The two failures that actually ended watches, verbatim from the logs. They
// are the reason retry exists, so they are the reason it is tested.
const (
	lidClosed = `gh pr view 68 --json headRefOid: exit status 1: Post "https://api.github.com/graphql": read tcp 192.168.1.69:63128->140.82.121.6:443: read: connection reset by peer`
	gatewayUp = `gh api repos/{owner}/{repo}/pulls/49/reviews: exit status 1: HTTP 502: Bad gateway`
)

// retryHarness records what retry did: how many attempts, how long it slept,
// and what it announced.
type retryHarness struct {
	attempts int
	slept    []time.Duration
	notes    []string
}

func (h *retryHarness) sleep(d time.Duration) { h.slept = append(h.slept, d) }

func (h *retryHarness) note(err error, next time.Duration) {
	h.notes = append(h.notes, fmt.Sprintf("%s: %v", next, err))
}

// failTimes returns a call that fails with err the first n times, then
// succeeds with "ok".
func (h *retryHarness) failTimes(n int, err error) func() (string, error) {
	return func() (string, error) {
		h.attempts++
		if h.attempts <= n {
			return "", err
		}
		return "ok", nil
	}
}

func TestRetrySurvivesALidClose(t *testing.T) {
	h := &retryHarness{}

	got, err := retry(h.failTimes(1, errors.New(lidClosed)), transientDelays, h.sleep, h.note)
	if err != nil {
		t.Fatalf("retry returned an error: %v", err)
	}
	if got != "ok" {
		t.Fatalf("value = %q, want ok", got)
	}

	// One failure, one wait, one more attempt — the whole point: the watch
	// resumes on wake instead of ending in the night.
	if h.attempts != 2 {
		t.Fatalf("attempts = %d, want 2", h.attempts)
	}
	if len(h.slept) != 1 || h.slept[0] != transientDelays[0] {
		t.Fatalf("slept = %v, want one %s", h.slept, transientDelays[0])
	}
	if len(h.notes) != 1 || !strings.Contains(h.notes[0], "connection reset by peer") {
		t.Fatalf("retry was not announced: %v", h.notes)
	}
}

func TestRetryWalksTheWholeScheduleBeforeGivingUp(t *testing.T) {
	h := &retryHarness{}
	boom := errors.New(gatewayUp)

	_, err := retry(h.failTimes(99, boom), transientDelays, h.sleep, h.note)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the last failure", err)
	}

	// len(delays)+1 attempts: one per delay, plus the final try that has no
	// delay left behind it.
	if want := len(transientDelays) + 1; h.attempts != want {
		t.Fatalf("attempts = %d, want %d", h.attempts, want)
	}
	if len(h.slept) != len(transientDelays) {
		t.Fatalf("slept %d times, want %d", len(h.slept), len(transientDelays))
	}
	for i, d := range transientDelays {
		if h.slept[i] != d {
			t.Fatalf("sleep %d = %s, want %s", i, h.slept[i], d)
		}
	}
}

// A terminal failure must cost exactly one attempt. Retrying a 404 would
// trade a clear error for a stall — the failure mode this change must not
// introduce while fixing the other one.
func TestRetryDoesNotRetryATerminalFailure(t *testing.T) {
	h := &retryHarness{}
	dead := errors.New(`gh pr view 999 --json state: exit status 1: HTTP 404: Not Found`)

	if _, err := retry(h.failTimes(99, dead), transientDelays, h.sleep, h.note); !errors.Is(err, dead) {
		t.Fatalf("error = %v, want the 404", err)
	}
	if h.attempts != 1 {
		t.Fatalf("attempts = %d, want 1 — a 404 will read the same in 40s", h.attempts)
	}
	if len(h.slept) != 0 {
		t.Fatalf("slept %v on a terminal failure", h.slept)
	}
}

func TestRetrySucceedsWithoutSleepingWhenNothingFails(t *testing.T) {
	h := &retryHarness{}

	got, err := retry(h.failTimes(0, nil), transientDelays, h.sleep, h.note)
	if err != nil || got != "ok" {
		t.Fatalf("got (%q, %v), want (ok, nil)", got, err)
	}
	if h.attempts != 1 || len(h.slept) != 0 {
		t.Fatalf("attempts = %d, slept = %v; want a single clean call", h.attempts, h.slept)
	}
}

func TestTransientClassification(t *testing.T) {
	cases := []struct {
		name string
		err  string
		want bool
	}{
		{"lid closed mid-poll (PR #68)", lidClosed, true},
		{"github bad gateway (PR #49)", gatewayUp, true},
		{"dns gone after wake", `gh api: exit status 1: dial tcp: lookup api.github.com: no such host`, true},
		{"wifi not back yet", `gh api: exit status 1: dial tcp 140.82.121.6:443: connect: network is unreachable`, true},
		{"stalled handshake", `gh api: exit status 1: net/http: TLS handshake timeout`, true},
		{"service unavailable", `gh api: exit status 1: HTTP 503: Service Unavailable`, true},

		{"deleted pr", `gh pr view 999: exit status 1: HTTP 404: Not Found`, false},
		{"expired token", `gh api: exit status 1: HTTP 401: Bad credentials`, false},
		{"no permission", `gh api: exit status 1: HTTP 403: Resource not accessible`, false},
		{"unprocessable request", `gh api: exit status 1: HTTP 422: Validation Failed`, false},
		{"not a repo", `gh pr view: exit status 1: no git remotes found`, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := transient(errors.New(c.err)); got != c.want {
				t.Fatalf("transient(%q) = %v, want %v", c.err, got, c.want)
			}
		})
	}

	if transient(nil) {
		t.Fatal("transient(nil) = true; a success is not a retry")
	}
}

// fakeGHOnPath puts an executable named gh at the front of PATH. It fails
// with the given stderr until a marker file has been written failures times,
// then succeeds with stdout — so the retry can be exercised through the real
// exec path without a network or a real gh.
func fakeGHOnPath(t *testing.T, failures int, stderr, stdout string) {
	t.Helper()

	// The space is deliberate. The script interpolates this path, and shell
	// quoting is exactly the kind of thing that regresses unnoticed — macOS
	// temp paths have no spaces, so without one here the guard never fires.
	dir := filepath.Join(t.TempDir(), "dir with space")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	script := fmt.Sprintf(`#!/bin/sh
count=$(cat "%[1]s/count" 2>/dev/null || echo 0)
count=$((count + 1))
echo $count > "%[1]s/count"
if [ "$count" -le %[2]d ]; then
  echo '%[3]s' >&2
  exit 1
fi
printf '%%s' '%[4]s'
`, dir, failures, stderr, stdout)

	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The helper being correct is not the same as the seam using it. Without
// this, unwiring run() from retry would leave every other test green and
// silently restore the bug the PR exists to fix.
func TestRunRetriesThroughTheRealExecPath(t *testing.T) {
	fakeGHOnPath(t, 1, "dial tcp 140.82.121.6:443: connect: network is unreachable", `{"state":"OPEN"}`)

	var slept []time.Duration
	var notes []string
	g := ghCLI{
		sleep: func(d time.Duration) { slept = append(slept, d) },
		note:  func(f string, a ...any) { notes = append(notes, fmt.Sprintf(f, a...)) },
	}

	out, err := g.run("pr", "view", "1", "--json", "state")
	if err != nil {
		t.Fatalf("run returned an error instead of retrying: %v", err)
	}
	if string(out) != `{"state":"OPEN"}` {
		t.Fatalf("stdout = %q, want the second attempt's output", out)
	}
	if len(slept) != 1 || slept[0] != transientDelays[0] {
		t.Fatalf("slept = %v, want one %s", slept, transientDelays[0])
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "network is unreachable") {
		t.Fatalf("retry was not announced: %v", notes)
	}
}

// gh api prints the response body to stderr pretty-printed, so a failing
// call carries newlines into the error — and most of the watcher's calls go
// through gh api. One retry must still be one log line, or the running
// commentary stops being greppable.
func TestRetryNoticeStaysOneLine(t *testing.T) {
	stderr := `{
  "message": "Bad gateway",
  "status": "502"
}gh: connection reset by peer`

	fakeGHOnPath(t, 1, stderr, "ok")

	var notes []string
	g := ghCLI{
		sleep: func(time.Duration) {},
		note:  func(f string, a ...any) { notes = append(notes, fmt.Sprintf(f, a...)) },
	}

	if _, err := g.run("api", "whatever"); err != nil {
		t.Fatalf("run returned an error instead of retrying: %v", err)
	}
	if len(notes) != 1 {
		t.Fatalf("notes = %v, want exactly one", notes)
	}
	if strings.Contains(notes[0], "\n") {
		t.Fatalf("retry notice spans multiple lines:\n%s", notes[0])
	}
	// Flattened, not truncated to uselessness: the reason still reads.
	if !strings.Contains(notes[0], "connection reset by peer") {
		t.Fatalf("retry notice lost the cause: %s", notes[0])
	}
}

// A zero-value ghCLI was the only shape this type had before the retry, and
// still reads as valid. Its hooks must default rather than panic — and the
// panic would land on the network-failure path, the one the retry exists to
// make dependable. Asserted on the accessors directly: going through run()
// would mean a real 2s sleep in a package that otherwise finishes in a
// fraction of that.
func TestZeroValueGhCLIHasUsableHooks(t *testing.T) {
	var g ghCLI

	if g.sleeper() == nil || g.noter() == nil {
		t.Fatal("zero-value ghCLI has nil hooks; run() would panic mid-retry")
	}

	// Calling them is the actual claim — a non-nil func that panics is no
	// better. Zero duration keeps it instant.
	g.sleeper()(0)
	g.noter()("retrying in %s: %v", time.Second, errors.New("boom"))
}

// The schedule has to outlast a wake, not just a hiccup: WiFi can take tens
// of seconds to reassociate, and a watch that gives up first is no better
// than one that never retried.
func TestTransientScheduleOutlastsAWake(t *testing.T) {
	var total time.Duration
	for _, d := range transientDelays {
		total += d
	}
	if total < time.Minute {
		t.Fatalf("retry schedule totals %s, too short to cover a network coming back", total)
	}
}
