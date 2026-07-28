package main

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

type config struct {
	pr            int
	interval      time.Duration
	grace         time.Duration
	awaitReviewer string
	awaitTimeout  time.Duration
	requestReview bool
	untilMerged   bool
	rerunFlake    string
	maxReruns     int
}

// watcher polls a PR until it settles — and, with untilMerged, on through
// to the merge. now/sleep are injected so tests can run the state machine
// on a virtual clock.
type watcher struct {
	gh    github
	cfg   config
	out   io.Writer
	now   func() time.Time
	sleep func(time.Duration)

	seen      map[string]bool
	requested map[string]bool

	// rerunsUsed counts flake reruns across the whole watch, not per head:
	// a flake that keeps recurring is not a flake, and the bound is what
	// keeps a watch from feeding a broken run forever.
	rerunsUsed int

	// Empty means the identity could not be resolved, and then nothing is
	// filtered — an unknown login must not become a login of "".
	viewerLogin string
}

func (w *watcher) log(format string, args ...any) {
	logf(w.out, w.now, format, args...)
}

// logf is the one log-line shape, shared with the gh seam's retry notices so
// a retry reads as part of the same running commentary.
func logf(out io.Writer, now func() time.Time, format string, args ...any) {
	fmt.Fprintf(out, "%s %s\n", now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

// run drives the watch to completion. Exit meaning: 0 = settled all-green
// (and merged, with untilMerged), 1 = settled with failing checks, or the
// PR closed unmerged.
func (w *watcher) run() (int, error) {
	head, err := w.gh.head(w.cfg.pr)
	if err != nil {
		return 1, err
	}
	w.log("watching PR #%d at %s", w.cfg.pr, head)

	// Summon the reviewer before waiting on anything — their pass rides in
	// parallel with CI instead of after it.
	w.requested = map[string]bool{}
	if err := w.maybeRequestReview(head); err != nil {
		return 1, err
	}

	if err := w.waitChecksRegistered(head); err != nil {
		return 1, err
	}

	w.resolveViewerLogin()

	// Baseline: existing activity is history, not news.
	w.seen = map[string]bool{}
	if _, err := w.newActivity(); err != nil {
		return 1, err
	}
	w.log("existing activity baselined (%d items) - only NEW items will be shown", len(w.seen))

	start := w.now()
	lastActivity := w.now()
	ready := false

	for {
		state, err := w.gh.prState(w.cfg.pr)
		if err != nil {
			return 1, err
		}
		switch state {
		case "MERGED":
			w.log("merged")
			return 0, nil
		case "CLOSED":
			w.log("closed without merge")
			return 1, nil
		}

		// A push mid-watch moves the head: re-pin and start over on the
		// new commit — checks, review and grace all belong to a head.
		h, err := w.gh.head(w.cfg.pr)
		if err != nil {
			return 1, err
		}
		if h != head {
			head = h
			w.log("head moved to %s - repinning", head)
			if err := w.maybeRequestReview(head); err != nil {
				return 1, err
			}
			if err := w.waitChecksRegistered(head); err != nil {
				return 1, err
			}
			start = w.now()
			lastActivity = w.now()
			ready = false
			continue
		}

		fresh, err := w.newActivity()
		if err != nil {
			return 1, err
		}
		for _, line := range fresh {
			w.log("NEW %s", line)
		}
		if len(fresh) > 0 {
			lastActivity = w.now()
			ready = false
		}

		// Once ready, only merge/close, a push, or new activity can change
		// anything — re-evaluating checks and the awaited review would just
		// burn API calls and repeat log lines.
		if ready {
			w.sleep(w.cfg.interval)
			continue
		}

		checks, err := w.gh.checks(w.cfg.pr)
		if err != nil {
			return 1, err
		}
		// No checks at all is the just-pushed window (the new head's runs
		// haven't registered yet) — never a settleable state.
		if len(checks) == 0 || pending(checks) > 0 {
			w.sleep(w.cfg.interval)
			continue
		}

		// A failing check may be a known flake: when its run log carries
		// the configured marker, the failed jobs are rerun instead of the
		// watch settling red. Only a triggered rerun keeps the loop
		// waiting — a genuine failure falls through and settles. (The bash
		// ancestor waited forever here whenever a real failure still had
		// rerun budget left.)
		if w.rerunFlakes(checks) {
			w.sleep(w.cfg.interval)
			continue
		}

		if w.now().Sub(lastActivity) < w.cfg.grace {
			w.sleep(w.cfg.interval)
			continue
		}

		if w.cfg.awaitReviewer != "" {
			reviewed, err := w.gh.reviewedBy(w.cfg.pr, w.cfg.awaitReviewer, head)
			if err != nil {
				return 1, err
			}
			if !reviewed {
				if w.now().Sub(start) < w.cfg.awaitTimeout {
					w.sleep(w.cfg.interval)
					continue
				}
				w.log("no review from %s on %s after %s - settling without it",
					w.cfg.awaitReviewer, head, w.cfg.awaitTimeout)
			}
		}

		// Failing checks need attention now, not after a merge that
		// shouldn't happen — exit 1 immediately even in until-merged mode,
		// keeping the documented exit contract (0 = all-green).
		if !w.cfg.untilMerged || hasFailures(checks) {
			return w.settle(checks), nil
		}
		ready = true
		w.log("ready: %s - waiting for merge", tally(checks))
		w.sleep(w.cfg.interval)
	}
}

// maybeRequestReview summons the awaited reviewer onto the current head,
// at most once per head. Every push invalidates the previous review, and
// for reviewers like Copilot the request is the only trigger — nothing on
// the GitHub side re-reviews a new head by itself. A head that already has
// its review is left alone (a watch started late must not summon a rerun).
func (w *watcher) maybeRequestReview(head string) error {
	if !w.cfg.requestReview || w.requested[head] {
		return nil
	}
	w.requested[head] = true

	reviewed, err := w.gh.reviewedBy(w.cfg.pr, w.cfg.awaitReviewer, head)
	if err != nil {
		return err
	}
	if reviewed {
		return nil
	}

	if err := w.gh.requestReview(w.cfg.pr, w.cfg.awaitReviewer); err != nil {
		// The watch stays useful without the request — the reviewer can be
		// summoned by hand — so report loudly and keep going. requested is
		// already marked: retrying a failing call every poll helps nobody.
		w.log("review request to %s FAILED - request it manually: %v", w.cfg.awaitReviewer, err)
		return nil
	}

	w.log("requested review from %s on %s", w.cfg.awaitReviewer, head)
	return nil
}

// rerunFlakes checks every failing run's log for the flake marker and reruns
// the matching ones' failed jobs. It reports whether any rerun was set in
// motion — the caller keeps waiting only then, so a genuine failure still
// settles promptly. A rerun request that errors ("already rerunning" when a
// previous trigger is still spinning up) counts as in motion: waiting is
// right either way, and the budget was already spent on the attempt.
func (w *watcher) rerunFlakes(checks []check) bool {
	if w.cfg.rerunFlake == "" {
		return false
	}

	triggered := false
	for _, id := range failedRunIDs(checks) {
		if w.rerunsUsed >= w.cfg.maxReruns {
			break
		}
		runLog, err := w.gh.runLog(id)
		if err != nil {
			// The watch stays useful without the rerun — report loudly and
			// let the failure settle on whatever the checks say.
			w.log("could not read the log of failed run %s: %v", id, err)
			continue
		}
		if !strings.Contains(runLog, w.cfg.rerunFlake) {
			continue
		}
		w.rerunsUsed++
		triggered = true
		w.log("flake marker in run %s - rerunning failed jobs (%d/%d)", id, w.rerunsUsed, w.cfg.maxReruns)
		if err := w.gh.rerunFailed(id); err != nil {
			w.log("rerun request for run %s FAILED (already rerunning?): %v", id, err)
		}
	}
	return triggered
}

// settle reports the final check state and maps it to the exit code.
func (w *watcher) settle(checks []check) int {
	w.log("settled: %s", tally(checks))
	if w.printFailures(checks) {
		return 1
	}
	return 0
}

func (w *watcher) printFailures(checks []check) bool {
	failed := false
	for _, c := range checks {
		if c.Bucket == "fail" {
			failed = true
			fmt.Fprintf(w.out, "  FAILED: %s  %s\n", c.Name, c.Link)
		}
	}
	return failed
}

func (w *watcher) waitChecksRegistered(sha string) error {
	for {
		ok, err := w.gh.checksRegistered(sha)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		w.log("no checks registered for the head yet, waiting...")
		w.sleep(w.cfg.interval)
	}
}

// resolveViewerLogin learns whose activity is this watch's own. A failure is
// NOT treated as "nobody" — an unknown identity means everything is reported,
// the behaviour this had before, rather than a silent filter keyed on "".
func (w *watcher) resolveViewerLogin() {
	login, err := w.gh.viewerLogin()
	if err != nil {
		w.log("could not ask who this watch is authenticated as (%v) - reporting all activity, including its own", err)
		return
	}

	// Trimmed here as well as in the seam: an all-whitespace login is not a
	// login, and filtering on one would swallow every item whose author
	// matched it (Copilot, PR #119).
	if login = strings.TrimSpace(login); login == "" {
		w.log("the authenticated login came back empty - reporting all activity, including this watch's own")
		return
	}

	w.viewerLogin = login
	w.log("ignoring own activity as %s", login)
}

// newActivity returns the not-yet-seen activity lines and marks them seen.
func (w *watcher) newActivity() ([]string, error) {
	items, err := w.gh.activity(w.cfg.pr)
	if err != nil {
		return nil, err
	}

	var fresh []string
	for _, it := range items {
		if w.seen[it.Key] {
			continue
		}
		// Marked seen either way: an own comment is history the moment it is
		// read, and re-examining it every poll would be work with one answer.
		w.seen[it.Key] = true

		// Replying is not news, and — the part that matters — it must not
		// reset the settle grace either, or a watch never settles while its
		// own author is answering threads.
		if w.viewerLogin != "" && it.Author == w.viewerLogin {
			continue
		}
		fresh = append(fresh, it.Text)
	}
	return fresh, nil
}

func pending(checks []check) int {
	n := 0
	for _, c := range checks {
		if c.Bucket == "pending" {
			n++
		}
	}
	return n
}

func hasFailures(checks []check) bool {
	for _, c := range checks {
		if c.Bucket == "fail" {
			return true
		}
	}
	return false
}

var runIDPattern = regexp.MustCompile(`/runs/([0-9]+)`)

// failedRunIDs extracts the workflow-run ids behind the failing checks from
// their details links, deduplicated, in check order. Checks that link
// somewhere else (external CI, no link at all) simply contribute nothing —
// there is no run to rerun.
func failedRunIDs(checks []check) []string {
	var ids []string
	seen := map[string]bool{}
	for _, c := range checks {
		if c.Bucket != "fail" {
			continue
		}
		m := runIDPattern.FindStringSubmatch(c.Link)
		if m == nil || seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		ids = append(ids, m[1])
	}
	return ids
}

// tally summarizes checks by bucket, sorted for stable output:
// "fail 1  pass 2". No trailing whitespace — callers own their spacing.
func tally(checks []check) string {
	counts := map[string]int{}
	for _, c := range checks {
		counts[c.Bucket]++
	}

	buckets := make([]string, 0, len(counts))
	for b := range counts {
		buckets = append(buckets, b)
	}
	sort.Strings(buckets)

	parts := make([]string, 0, len(buckets))
	for _, b := range buckets {
		parts = append(parts, fmt.Sprintf("%s %d", b, counts[b]))
	}
	return strings.Join(parts, "  ")
}
