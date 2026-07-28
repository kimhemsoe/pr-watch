package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeGH scripts the GitHub seam with closures; unset fields get quiet
// defaults (open PR, one passing check, no activity).
type fakeGH struct {
	headFn        func() string
	stateFn       func() string
	checksFn      func() []check
	activityFn    func() []item
	reviewedFn    func(author, sha string) bool
	requestFn     func(reviewer string) error
	viewerLoginFn func() (string, error)
	runLogFn      func(id string) (string, error)
	rerunFn       func(id string) error
}

func (f fakeGH) currentPR() (int, error) { return 1, nil }
func (f fakeGH) head(int) (string, error) {
	if f.headFn == nil {
		return "head1", nil
	}
	return f.headFn(), nil
}
func (f fakeGH) prState(int) (string, error) {
	if f.stateFn == nil {
		return "OPEN", nil
	}
	return f.stateFn(), nil
}
func (f fakeGH) checks(int) ([]check, error) {
	if f.checksFn == nil {
		return []check{{Name: "check", Bucket: "pass"}}, nil
	}
	return f.checksFn(), nil
}
func (f fakeGH) checksRegistered(string) (bool, error) { return true, nil }
func (f fakeGH) viewerLogin() (string, error) {
	if f.viewerLoginFn == nil {
		return "watcher-self", nil
	}
	return f.viewerLoginFn()
}
func (f fakeGH) activity(int) ([]item, error) {
	if f.activityFn == nil {
		return nil, nil
	}
	return f.activityFn(), nil
}
func (f fakeGH) reviewedBy(_ int, author, sha string) (bool, error) {
	if f.reviewedFn == nil {
		return false, nil
	}
	return f.reviewedFn(author, sha), nil
}
func (f fakeGH) requestReview(_ int, reviewer string) error {
	if f.requestFn == nil {
		return nil
	}
	return f.requestFn(reviewer)
}
func (f fakeGH) runLog(id string) (string, error) {
	if f.runLogFn == nil {
		return "", nil
	}
	return f.runLogFn(id)
}
func (f fakeGH) rerunFailed(id string) error {
	if f.rerunFn == nil {
		return nil
	}
	return f.rerunFn(id)
}

// testClock is the virtual time the watcher runs on: sleep advances it, so
// scenarios describe events by elapsed time instead of real waiting.
type testClock struct {
	t    *testing.T
	base time.Time
	now  time.Time
}

func newTestClock(t *testing.T) *testClock {
	base := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	return &testClock{t: t, base: base, now: base}
}

func (c *testClock) elapsed() time.Duration { return c.now.Sub(c.base) }

func (c *testClock) sleep(d time.Duration) {
	c.now = c.now.Add(d)
	if c.elapsed() > 24*time.Hour {
		c.t.Fatal("watch never settled (virtual clock passed 24h)")
	}
}

func runWatch(t *testing.T, gh github, cfg config) (int, string, *testClock) {
	t.Helper()
	clk := newTestClock(t)
	out := &bytes.Buffer{}
	w := &watcher{gh: gh, cfg: cfg, out: out,
		now:   func() time.Time { return clk.now },
		sleep: clk.sleep,
	}
	code, err := w.run()
	if err != nil {
		t.Fatal(err)
	}
	return code, out.String(), clk
}

func baseConfig() config {
	return config{pr: 1, interval: 30 * time.Second, grace: 240 * time.Second, awaitTimeout: 1800 * time.Second}
}

func TestSettlesAfterQuietGrace(t *testing.T) {
	code, out, clk := runWatch(t, fakeGH{}, baseConfig())
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if !strings.Contains(out, "settled: pass 1") {
		t.Fatalf("missing settle line:\n%s", out)
	}
	if e := clk.elapsed(); e < 240*time.Second {
		t.Fatalf("settled before the grace period: %s", e)
	}
}

func TestFailedCheckExitsNonzero(t *testing.T) {
	gh := fakeGH{checksFn: func() []check {
		return []check{{Name: "check", Bucket: "fail", Link: "https://ci/run/1"}}
	}}
	code, out, _ := runWatch(t, gh, baseConfig())
	if code != 1 {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if !strings.Contains(out, "FAILED: check") {
		t.Fatalf("missing failure line:\n%s", out)
	}
}

// The happy flake path: the failing run's log carries the marker, the failed
// jobs are rerun, the rerun goes green, the watch settles 0 — no human woken.
func TestFlakeRerunRecoversAndSettlesGreen(t *testing.T) {
	clk := newTestClock(t)
	rerunAt := time.Duration(-1)
	var reruns []string
	gh := fakeGH{
		checksFn: func() []check {
			switch {
			case rerunAt < 0:
				return []check{{Name: "test", Bucket: "fail", Link: "https://github.com/o/r/actions/runs/42/job/7"}}
			case clk.elapsed() < rerunAt+2*time.Minute:
				return []check{{Name: "test", Bucket: "pending"}}
			default:
				return []check{{Name: "test", Bucket: "pass"}}
			}
		},
		runLogFn: func(id string) (string, error) {
			return "boot\nFailed to initialize container localstack\n", nil
		},
		rerunFn: func(id string) error {
			reruns = append(reruns, id)
			rerunAt = clk.elapsed()
			return nil
		},
	}

	cfg := baseConfig()
	cfg.rerunFlake = "Failed to initialize container localstack"
	cfg.maxReruns = 2

	out := &bytes.Buffer{}
	w := &watcher{gh: gh, cfg: cfg, out: out,
		now:   func() time.Time { return clk.now },
		sleep: clk.sleep,
	}
	code, err := w.run()
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out.String())
	}
	if len(reruns) != 1 || reruns[0] != "42" {
		t.Fatalf("reruns = %v, want [42]", reruns)
	}
	if !strings.Contains(out.String(), "flake marker in run 42 - rerunning failed jobs (1/2)") {
		t.Fatalf("rerun not narrated:\n%s", out.String())
	}
}

// RULE: only a triggered rerun keeps the watch waiting. A genuine failure —
// no marker in the log — settles red even with the whole rerun budget left.
// (The bash ancestor of this tool waited forever on exactly this state.)
func TestGenuineFailureSettlesRedWithBudgetLeft(t *testing.T) {
	gh := fakeGH{
		checksFn: func() []check {
			return []check{{Name: "test", Bucket: "fail", Link: "https://github.com/o/r/actions/runs/42/job/7"}}
		},
		runLogFn: func(id string) (string, error) {
			return "assertion failed: expected 1, got 2", nil
		},
		rerunFn: func(id string) error {
			t.Fatal("rerun triggered without the flake marker")
			return nil
		},
	}

	cfg := baseConfig()
	cfg.rerunFlake = "Failed to initialize container localstack"
	cfg.maxReruns = 2

	code, out, _ := runWatch(t, gh, cfg)
	if code != 1 {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if !strings.Contains(out, "FAILED: test") {
		t.Fatalf("missing failure line:\n%s", out)
	}
}

// A flake that keeps recurring is not a flake: the budget bounds the watch's
// patience, and once spent the persistent failure settles red.
func TestFlakeRerunIsBounded(t *testing.T) {
	reruns := 0
	gh := fakeGH{
		checksFn: func() []check {
			return []check{{Name: "test", Bucket: "fail", Link: "https://github.com/o/r/actions/runs/42/job/7"}}
		},
		runLogFn: func(id string) (string, error) {
			return "Failed to initialize container localstack", nil
		},
		rerunFn: func(id string) error { reruns++; return nil },
	}

	cfg := baseConfig()
	cfg.rerunFlake = "Failed to initialize container localstack"
	cfg.maxReruns = 2

	code, out, _ := runWatch(t, gh, cfg)
	if code != 1 {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if reruns != 2 {
		t.Fatalf("reruns = %d, want exactly the budget of 2", reruns)
	}
	if !strings.Contains(out, "rerunning failed jobs (2/2)") {
		t.Fatalf("budget not narrated:\n%s", out)
	}
}

// An unreadable run log must not kill or stall the watch: the failure is
// reported and the checks settle as what they are.
func TestUnreadableRunLogSettlesRed(t *testing.T) {
	gh := fakeGH{
		checksFn: func() []check {
			return []check{{Name: "test", Bucket: "fail", Link: "https://github.com/o/r/actions/runs/42/job/7"}}
		},
		runLogFn: func(id string) (string, error) { return "", errors.New("HTTP 410: log expired") },
		rerunFn: func(id string) error {
			t.Fatal("rerun triggered off an unreadable log")
			return nil
		},
	}

	cfg := baseConfig()
	cfg.rerunFlake = "MARKER"
	cfg.maxReruns = 2

	code, out, _ := runWatch(t, gh, cfg)
	if code != 1 {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if !strings.Contains(out, "could not read the log of failed run 42") {
		t.Fatalf("log failure not reported:\n%s", out)
	}
}

func TestFailedRunIDsDedupesAndSkipsForeignLinks(t *testing.T) {
	ids := failedRunIDs([]check{
		{Bucket: "fail", Link: "https://github.com/o/r/actions/runs/42/job/1"},
		{Bucket: "fail", Link: "https://github.com/o/r/actions/runs/42/job/2"},
		{Bucket: "fail", Link: "https://github.com/o/r/actions/runs/99/job/1"},
		{Bucket: "pass", Link: "https://github.com/o/r/actions/runs/7/job/1"},
		{Bucket: "fail", Link: ""},
		{Bucket: "fail", Link: "https://external-ci.example/build/5"},
	})
	if len(ids) != 2 || ids[0] != "42" || ids[1] != "99" {
		t.Fatalf("ids = %v, want [42 99]", ids)
	}
}

// The race that motivated --await-review: the reviewer posts 15 minutes
// after checks pass, far beyond any quiet grace. The watch must hold.
func TestAwaitReviewHoldsThroughSilence(t *testing.T) {
	clkRef := &struct{ c *testClock }{}
	gh := fakeGH{
		reviewedFn: func(author, sha string) bool {
			return author == "bot" && clkRef.c.elapsed() >= 15*time.Minute
		},
	}

	cfg := baseConfig()
	cfg.awaitReviewer = "bot"

	clk := newTestClock(t)
	clkRef.c = clk
	out := &bytes.Buffer{}
	w := &watcher{gh: gh, cfg: cfg, out: out,
		now:   func() time.Time { return clk.now },
		sleep: clk.sleep,
	}
	code, err := w.run()
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out.String())
	}
	if e := clk.elapsed(); e < 15*time.Minute {
		t.Fatalf("settled before the review arrived: %s", e)
	}
}

func TestAwaitReviewTimesOut(t *testing.T) {
	cfg := baseConfig()
	cfg.awaitReviewer = "bot"
	cfg.awaitTimeout = 10 * time.Minute

	code, out, clk := runWatch(t, fakeGH{}, cfg)
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if !strings.Contains(out, "settling without it") {
		t.Fatalf("missing timeout warning:\n%s", out)
	}
	if e := clk.elapsed(); e < 10*time.Minute {
		t.Fatalf("gave up before the timeout: %s", e)
	}
}

func TestNewActivityResetsGrace(t *testing.T) {
	clk := newTestClock(t)
	gh := fakeGH{activityFn: func() []item {
		if clk.elapsed() >= 2*time.Minute {
			return []item{{Key: "c1", Text: "COMMENT (bot) a.go:1 id:1: hm"}}
		}
		return nil
	}}

	out := &bytes.Buffer{}
	w := &watcher{gh: gh, cfg: baseConfig(), out: out,
		now:   func() time.Time { return clk.now },
		sleep: clk.sleep,
	}
	code, err := w.run()
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "NEW COMMENT (bot)") {
		t.Fatalf("new activity not reported:\n%s", out.String())
	}
	// The comment landed at 2min; grace must restart from there.
	if e := clk.elapsed(); e < 2*time.Minute+240*time.Second {
		t.Fatalf("grace did not reset on activity: settled at %s", e)
	}
}

// A push mid-watch moves the head: the watch re-pins and the awaited
// review must be for the NEW head, not the old one.
func TestPushRepinsAndReviewTargetsNewHead(t *testing.T) {
	clk := newTestClock(t)
	type query struct {
		sha string
		at  time.Duration
	}
	var queries []query
	gh := fakeGH{
		headFn: func() string {
			if clk.elapsed() >= 5*time.Minute {
				return "head2"
			}
			return "head1"
		},
		reviewedFn: func(author, sha string) bool {
			queries = append(queries, query{sha, clk.elapsed()})
			return sha == "head2" && clk.elapsed() >= 20*time.Minute
		},
	}

	cfg := baseConfig()
	cfg.awaitReviewer = "bot"

	out := &bytes.Buffer{}
	w := &watcher{gh: gh, cfg: cfg, out: out,
		now:   func() time.Time { return clk.now },
		sleep: clk.sleep,
	}
	code, err := w.run()
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "head moved to head2 - repinning") {
		t.Fatalf("repin not logged:\n%s", out.String())
	}
	for _, q := range queries {
		if q.sha == "head1" && q.at >= 6*time.Minute {
			t.Fatalf("still asking about the old head at %s, after the push", q.at)
		}
	}
	if e := clk.elapsed(); e < 20*time.Minute {
		t.Fatalf("settled before the new head's review: %s", e)
	}
}

// The full request-review loop: the watcher summons the reviewer at start,
// the review lands, a push moves the head — and the NEW head gets its own
// request, exactly one per head across all the polls in between.
func TestRequestReviewSummonsOncePerHead(t *testing.T) {
	clk := newTestClock(t)
	headAt := func() string {
		if clk.elapsed() >= 5*time.Minute {
			return "head2"
		}
		return "head1"
	}

	requested := map[string]time.Duration{}
	gh := fakeGH{
		headFn: headAt,
		stateFn: func() string {
			if clk.elapsed() >= 30*time.Minute {
				return "MERGED"
			}
			return "OPEN"
		},
		requestFn: func(reviewer string) error {
			if reviewer != "bot" {
				t.Fatalf("requested %q, want bot", reviewer)
			}
			requested[headAt()] = clk.elapsed()
			return nil
		},
		// A review lands three minutes after its head was requested —
		// never before, and never for an unrequested head.
		reviewedFn: func(author, sha string) bool {
			at, ok := requested[sha]
			return ok && clk.elapsed() >= at+3*time.Minute
		},
	}

	cfg := baseConfig()
	cfg.awaitReviewer = "bot"
	cfg.requestReview = true
	cfg.untilMerged = true

	out := &bytes.Buffer{}
	w := &watcher{gh: gh, cfg: cfg, out: out,
		now:   func() time.Time { return clk.now },
		sleep: clk.sleep,
	}
	code, err := w.run()
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out.String())
	}
	if len(requested) != 2 {
		t.Fatalf("requests per head: %v", requested)
	}
	if requested["head2"] < 5*time.Minute {
		t.Fatalf("head2 requested before it existed: %v", requested)
	}
	s := out.String()
	if !strings.Contains(s, "requested review from bot on head1") ||
		!strings.Contains(s, "requested review from bot on head2") {
		t.Fatalf("request lines missing:\n%s", s)
	}
}

// A watch started late finds the head already reviewed — summoning the
// reviewer again would trigger a pointless rerun.
func TestRequestReviewSkipsReviewedHead(t *testing.T) {
	requests := 0
	gh := fakeGH{
		reviewedFn: func(author, sha string) bool { return true },
		requestFn:  func(string) error { requests++; return nil },
	}

	cfg := baseConfig()
	cfg.awaitReviewer = "bot"
	cfg.requestReview = true

	code, out, _ := runWatch(t, gh, cfg)
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if requests != 0 {
		t.Fatalf("summoned a reviewer who already reviewed (%d times)", requests)
	}
}

// A failed request must not kill the watch or be retried every poll — it is
// reported once, and the await path degrades to its normal timeout.
func TestRequestReviewFailureKeepsWatching(t *testing.T) {
	attempts := 0
	gh := fakeGH{requestFn: func(string) error {
		attempts++
		return errors.New("HTTP 422")
	}}

	cfg := baseConfig()
	cfg.awaitReviewer = "bot"
	cfg.requestReview = true
	cfg.awaitTimeout = 10 * time.Minute

	code, out, _ := runWatch(t, gh, cfg)
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if attempts != 1 {
		t.Fatalf("failing request tried %d times, want 1", attempts)
	}
	if !strings.Contains(out, "FAILED - request it manually") {
		t.Fatalf("failure not reported:\n%s", out)
	}
	if !strings.Contains(out, "settling without it") {
		t.Fatalf("await did not degrade to its timeout:\n%s", out)
	}
}

// Without the flag, awaiting a review must never summon one.
func TestNoRequestWithoutTheFlag(t *testing.T) {
	requests := 0
	gh := fakeGH{requestFn: func(string) error { requests++; return nil }}

	cfg := baseConfig()
	cfg.awaitReviewer = "bot"
	cfg.awaitTimeout = 10 * time.Minute

	code, out, _ := runWatch(t, gh, cfg)
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if requests != 0 {
		t.Fatalf("requested a review without --request-review (%d times)", requests)
	}
}

func TestUntilMergedWaitsForTheMerge(t *testing.T) {
	clk := newTestClock(t)
	gh := fakeGH{stateFn: func() string {
		if clk.elapsed() >= 30*time.Minute {
			return "MERGED"
		}
		return "OPEN"
	}}

	cfg := baseConfig()
	cfg.untilMerged = true

	out := &bytes.Buffer{}
	w := &watcher{gh: gh, cfg: cfg, out: out,
		now:   func() time.Time { return clk.now },
		sleep: clk.sleep,
	}
	code, err := w.run()
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "ready: pass 1") || !strings.Contains(s, "merged") {
		t.Fatalf("missing ready/merged lines:\n%s", s)
	}
	if strings.Count(s, "ready:") != 1 {
		t.Fatalf("ready reported more than once:\n%s", s)
	}
}

// Failing checks exit 1 immediately even in until-merged mode — a merge of
// a red PR must not launder the exit code to 0 (Copilot review, PR #31).
func TestUntilMergedStillFailsOnRedChecks(t *testing.T) {
	gh := fakeGH{checksFn: func() []check {
		return []check{{Name: "check", Bucket: "fail", Link: "https://ci/run/2"}}
	}}

	cfg := baseConfig()
	cfg.untilMerged = true

	code, out, _ := runWatch(t, gh, cfg)
	if code != 1 {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if strings.Contains(out, "ready:") {
		t.Fatalf("red PR reported ready:\n%s", out)
	}
}

// Once ready, the await-timeout warning must not repeat every poll while
// waiting for the merge (Copilot review, PR #31).
func TestAwaitTimeoutWarnsOnceWhileWaitingForMerge(t *testing.T) {
	clk := newTestClock(t)
	gh := fakeGH{stateFn: func() string {
		if clk.elapsed() >= 2*time.Hour {
			return "MERGED"
		}
		return "OPEN"
	}}

	cfg := baseConfig()
	cfg.untilMerged = true
	cfg.awaitReviewer = "bot"
	cfg.awaitTimeout = 10 * time.Minute

	out := &bytes.Buffer{}
	w := &watcher{gh: gh, cfg: cfg, out: out,
		now:   func() time.Time { return clk.now },
		sleep: clk.sleep,
	}
	code, err := w.run()
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out.String())
	}
	if n := strings.Count(out.String(), "settling without it"); n != 1 {
		t.Fatalf("timeout warning logged %d times:\n%s", n, out.String())
	}
}

func TestClosedUnmergedExitsNonzero(t *testing.T) {
	clk := newTestClock(t)
	gh := fakeGH{stateFn: func() string {
		if clk.elapsed() >= 5*time.Minute {
			return "CLOSED"
		}
		return "OPEN"
	}}

	cfg := baseConfig()
	cfg.untilMerged = true

	out := &bytes.Buffer{}
	w := &watcher{gh: gh, cfg: cfg, out: out,
		now:   func() time.Time { return clk.now },
		sleep: clk.sleep,
	}
	code, err := w.run()
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 || !strings.Contains(out.String(), "closed without merge") {
		t.Fatalf("exit = %d\n%s", code, out.String())
	}
}

// The just-pushed window: a head with no registered checks yet must hold
// the watch, never settle it (regression: this crashed a live watch on
// wixzettle #33 via the strict gh error path, and settling on an empty
// tally would be worse).
func TestEmptyChecksHoldTheWatch(t *testing.T) {
	clk := newTestClock(t)
	gh := fakeGH{checksFn: func() []check {
		if clk.elapsed() >= 10*time.Minute {
			return []check{{Name: "check", Bucket: "pass"}}
		}
		return nil
	}}

	out := &bytes.Buffer{}
	w := &watcher{gh: gh, cfg: baseConfig(), out: out,
		now:   func() time.Time { return clk.now },
		sleep: clk.sleep,
	}
	code, err := w.run()
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out.String())
	}
	if e := clk.elapsed(); e < 10*time.Minute {
		t.Fatalf("settled during the empty-checks window: %s", e)
	}
}

func TestTallyIsStable(t *testing.T) {
	got := tally([]check{{Bucket: "pass"}, {Bucket: "fail"}, {Bucket: "pass"}})
	if got != "fail 1  pass 2" {
		t.Fatalf("tally = %q", got)
	}
}

func TestFlattenBoundsAndJoins(t *testing.T) {
	if got := flatten("a\nb\r\nc"); got != "a | b | c" {
		t.Fatalf("flatten = %q", got)
	}
	long := strings.Repeat("é", 500)
	if got := flatten(long); len([]rune(got)) != 400 {
		t.Fatalf("flatten cap = %d runes", len([]rune(got)))
	}
}

// RULE: the watch's own replies are not news to it. Answering review threads
// is the main thing this tool's operator does while waiting, and reporting it
// back drowns the feed — worse, it counts as activity and resets the settle
// grace, so a watch that is being replied to never settles.
func TestOwnActivityIsNeitherReportedNorGrace(t *testing.T) {
	clk := newTestClock(t)
	gh := fakeGH{activityFn: func() []item {
		if clk.elapsed() >= 2*time.Minute {
			return []item{{Key: "c1", Author: "watcher-self",
				Text: "COMMENT (watcher-self) a.go:1 id:1: replying to the review"}}
		}
		return nil
	}}

	out := &bytes.Buffer{}
	w := &watcher{gh: gh, cfg: baseConfig(), out: out,
		now:   func() time.Time { return clk.now },
		sleep: clk.sleep,
	}
	code, err := w.run()
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out.String())
	}
	if strings.Contains(out.String(), "NEW COMMENT (watcher-self)") {
		t.Errorf("own comment reported back as news:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "ignoring own activity as watcher-self") {
		t.Errorf("the filter must say it is on:\n%s", out.String())
	}
	// Settles on the ORIGINAL grace: the reply at 2min must not have restarted
	// it, which is the difference between this and TestNewActivityResetsGrace.
	if e := clk.elapsed(); e >= 2*time.Minute+240*time.Second {
		t.Errorf("own comment reset the grace: settled at %s", e)
	}
}

// RULE: filtering is by author, not by "anything that arrives late". Someone
// else's comment at the same moment still lands and still resets the grace.
func TestOtherAuthorsStillReported(t *testing.T) {
	clk := newTestClock(t)
	gh := fakeGH{activityFn: func() []item {
		if clk.elapsed() >= 2*time.Minute {
			return []item{
				{Key: "c1", Author: "watcher-self", Text: "COMMENT (watcher-self) a.go:1 id:1: mine"},
				{Key: "c2", Author: "Copilot", Text: "COMMENT (Copilot) a.go:2 id:2: theirs"},
			}
		}
		return nil
	}}

	out := &bytes.Buffer{}
	w := &watcher{gh: gh, cfg: baseConfig(), out: out,
		now:   func() time.Time { return clk.now },
		sleep: clk.sleep,
	}
	if _, err := w.run(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "NEW COMMENT (watcher-self)") {
		t.Errorf("own comment leaked:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "NEW COMMENT (Copilot)") {
		t.Fatalf("the other author's comment was swallowed:\n%s", out.String())
	}
	if e := clk.elapsed(); e < 2*time.Minute+240*time.Second {
		t.Errorf("a real comment must still reset the grace: settled at %s", e)
	}
}

// RULE: an identity that cannot be resolved is not an identity of "". Failing
// to learn who we are must report everything — the old behaviour — rather than
// filter on an empty login and silently swallow whoever matches it. The same
// absence-of-evidence rule the mirror side keeps relearning.
func TestUnknownViewerReportsEverything(t *testing.T) {
	clk := newTestClock(t)
	gh := fakeGH{
		viewerLoginFn: func() (string, error) { return "", errors.New("gh not authenticated") },
		activityFn: func() []item {
			// After the baseline, or it is history rather than news.
			if clk.elapsed() >= 2*time.Minute {
				return []item{{Key: "c1", Author: "", Text: "COMMENT () a.go:1 id:1: authorless"}}
			}
			return nil
		},
	}

	buf := &bytes.Buffer{}
	w := &watcher{gh: gh, cfg: baseConfig(), out: buf,
		now:   func() time.Time { return clk.now },
		sleep: clk.sleep,
	}
	if _, err := w.run(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "could not ask who this watch is authenticated as") {
		t.Errorf("the fallback must say why it is reporting everything:\n%s", out)
	}
	if !strings.Contains(out, "NEW COMMENT ()") {
		t.Errorf("an unknown viewer must not filter anything:\n%s", out)
	}
}

// RULE: a lookup that SUCCEEDS with nothing to say is its own case. It is not
// an error, so it must not be reported as one — and it is not a login, so it
// must not be filtered on (Copilot, PR #119).
func TestEmptyViewerReportsEverything(t *testing.T) {
	clk := newTestClock(t)
	gh := fakeGH{
		// Succeeds, answers whitespace: nil error, nothing usable.
		viewerLoginFn: func() (string, error) { return "  \n", nil },
		activityFn: func() []item {
			if clk.elapsed() >= 2*time.Minute {
				return []item{{Key: "c1", Author: "  \n", Text: "COMMENT ( ) a.go:1 id:1: whitespace author"}}
			}
			return nil
		},
	}

	buf := &bytes.Buffer{}
	w := &watcher{gh: gh, cfg: baseConfig(), out: buf,
		now:   func() time.Time { return clk.now },
		sleep: clk.sleep,
	}
	if _, err := w.run(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if strings.Contains(out, "<nil>") {
		t.Errorf("a successful lookup must not be reported as an error:\n%s", out)
	}
	if !strings.Contains(out, "the authenticated login came back empty") {
		t.Errorf("the empty case needs its own message:\n%s", out)
	}
	if !strings.Contains(out, "NEW COMMENT ( )") {
		t.Errorf("an empty viewer must filter nothing, not everything matching it:\n%s", out)
	}
}
