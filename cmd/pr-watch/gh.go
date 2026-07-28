package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// github is the access seam: the real implementation shells out to the gh
// CLI, so auth, host and owner/repo resolution stay gh's problem. Tests
// substitute a fake and drive the watcher's state machine directly.
type github interface {
	currentPR() (int, error)
	head(pr int) (string, error)
	prState(pr int) (string, error) // "OPEN", "MERGED" or "CLOSED"
	checks(pr int) ([]check, error)
	checksRegistered(sha string) (bool, error)
	activity(pr int) ([]item, error)
	viewerLogin() (string, error)
	reviewedBy(pr int, author, sha string) (bool, error)
	requestReview(pr int, reviewer string) error
	runLog(runID string) (string, error)
	rerunFailed(runID string) error
}

// check is one row of `gh pr checks`: bucket is gh's state word
// (pass, fail, pending, skipping).
type check struct {
	Name   string
	Bucket string
	Link   string
}

// item is one piece of PR activity with a stable key, so new items can be
// diffed against the already-seen set across polls. Author is carried apart
// from Text so the watch can tell whose activity it is without parsing its
// own message back out.
type item struct {
	Key    string
	Author string
	Text   string
}

// ghCLI shells out to the gh CLI. sleep and note exist for the retry: a
// watch is meant to survive unattended, so a call that fails for a reason
// that will not still be true in a minute is tried again rather than ending
// the watch (see retry below).
type ghCLI struct {
	sleep func(time.Duration)
	note  func(format string, args ...any)
}

func newGHCLI(out io.Writer, now func() time.Time) ghCLI {
	return ghCLI{
		sleep: time.Sleep,
		note: func(format string, args ...any) {
			logf(out, now, format, args...)
		},
	}
}

// run invokes gh once, retrying transient failures on the schedule. Every
// call the watcher makes is a read except requestReview, and re-requesting
// a reviewer GitHub already has is a no-op — so retrying is safe for all of
// them without an idempotency key.
func (g ghCLI) run(args ...string) ([]byte, error) {
	return retry(
		func() ([]byte, error) { return g.runOnce(args...) },
		transientDelays,
		g.sleeper(),
		// flatten, because gh api prints the response body to stderr and it
		// arrives pretty-printed — a raw %v would spill one retry across
		// five lines and break the one-line-per-event shape the log is read
		// (and grepped) by. The error itself stays untouched for
		// classification and for the caller.
		func(err error, next time.Duration) {
			g.noter()("transient gh failure, retrying in %s: %s", next, flatten(err.Error()))
		},
	)
}

// sleeper and noter default the injected hooks. Before the retry existed
// this type was `struct{}`, so `ghCLI{}` was the only way to build one and
// still reads as valid — without these it would panic on a nil func, on the
// one path the retry exists to make dependable. A zero value retries
// silently rather than not at all.
func (g ghCLI) sleeper() func(time.Duration) {
	if g.sleep == nil {
		return time.Sleep
	}
	return g.sleep
}

func (g ghCLI) noter() func(format string, args ...any) {
	if g.note == nil {
		return func(string, ...any) {}
	}
	return g.note
}

func (g ghCLI) runOnce(args ...string) ([]byte, error) {
	cmd := exec.Command("gh", args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		// Lenience is for `gh pr checks` only: it exits nonzero when
		// checks fail or are pending — that is data, not an error. Every
		// other command's failure must surface (a swallowed `gh api`
		// error would feed its error payload to json.Unmarshal).
		// Right after a push there is a window where the new head has no
		// checks at all; gh reports that as an error too, but for the
		// watcher it is just an empty (not-yet-registered) state.
		if len(args) > 1 && args[0] == "pr" && args[1] == "checks" &&
			(out.Len() > 0 || strings.Contains(errb.String(), "no checks reported")) {
			return out.Bytes(), nil
		}
		return nil, fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

func (g ghCLI) currentPR() (int, error) {
	out, err := g.run("pr", "view", "--json", "number")
	if err != nil {
		return 0, err
	}
	var v struct{ Number int }
	if err := json.Unmarshal(out, &v); err != nil {
		return 0, err
	}
	return v.Number, nil
}

func (g ghCLI) head(pr int) (string, error) {
	out, err := g.run("pr", "view", fmt.Sprint(pr), "--json", "headRefOid")
	if err != nil {
		return "", err
	}
	var v struct{ HeadRefOid string }
	if err := json.Unmarshal(out, &v); err != nil {
		return "", err
	}
	return v.HeadRefOid, nil
}

func (g ghCLI) prState(pr int) (string, error) {
	out, err := g.run("pr", "view", fmt.Sprint(pr), "--json", "state")
	if err != nil {
		return "", err
	}
	var v struct{ State string }
	if err := json.Unmarshal(out, &v); err != nil {
		return "", err
	}
	return v.State, nil
}

func (g ghCLI) checks(pr int) ([]check, error) {
	out, err := g.run("pr", "checks", fmt.Sprint(pr))
	if err != nil {
		return nil, err
	}

	var checks []check
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 2 {
			continue
		}
		c := check{Name: f[0], Bucket: f[1]}
		if len(f) > 3 {
			c.Link = f[3]
		}
		checks = append(checks, c)
	}
	return checks, nil
}

func (g ghCLI) checksRegistered(sha string) (bool, error) {
	out, err := g.run("api", "repos/{owner}/{repo}/commits/"+sha+"/check-runs", "--jq", ".total_count")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "0", nil
}

func (g ghCLI) activity(pr int) ([]item, error) {
	var items []item

	var comments []struct {
		ID           int64  `json:"id"`
		Path         string `json:"path"`
		Line         *int   `json:"line"`
		OriginalLine *int   `json:"original_line"`
		Body         string `json:"body"`
		User         struct{ Login string }
	}
	if err := g.getJSON(fmt.Sprintf("repos/{owner}/{repo}/pulls/%d/comments", pr), &comments); err != nil {
		return nil, err
	}
	for _, c := range comments {
		// Outdated comments can lose their line; ":0" is not a location,
		// so the suffix only appears when one is known.
		loc := c.Path
		if c.Line != nil && *c.Line > 0 {
			loc = fmt.Sprintf("%s:%d", c.Path, *c.Line)
		} else if c.OriginalLine != nil && *c.OriginalLine > 0 {
			loc = fmt.Sprintf("%s:%d", c.Path, *c.OriginalLine)
		}
		items = append(items, item{
			Key:    fmt.Sprintf("c%d", c.ID),
			Author: c.User.Login,
			Text:   fmt.Sprintf("COMMENT (%s) %s id:%d: %s", c.User.Login, loc, c.ID, flatten(c.Body)),
		})
	}

	var discussion []struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		User struct{ Login string }
	}
	if err := g.getJSON(fmt.Sprintf("repos/{owner}/{repo}/issues/%d/comments", pr), &discussion); err != nil {
		return nil, err
	}
	for _, c := range discussion {
		items = append(items, item{
			Key:    fmt.Sprintf("i%d", c.ID),
			Author: c.User.Login,
			Text:   fmt.Sprintf("DISCUSSION (%s): %s", c.User.Login, flatten(c.Body)),
		})
	}

	reviews, err := g.reviews(pr)
	if err != nil {
		return nil, err
	}
	for _, r := range reviews {
		if r.Body == "" {
			continue
		}
		items = append(items, item{
			Key:    fmt.Sprintf("v%d", r.ID),
			Author: r.User.Login,
			Text:   fmt.Sprintf("REVIEW %s (%s): %s", r.State, r.User.Login, flatten(r.Body)),
		})
	}

	return items, nil
}

type review struct {
	ID       int64  `json:"id"`
	State    string `json:"state"`
	Body     string `json:"body"`
	CommitID string `json:"commit_id"`
	User     struct{ Login string }
}

// viewerLogin answers the login the gh CLI is authenticated as — whose activity
// on the PR is therefore this watch's own doing rather than news.
func (g ghCLI) viewerLogin() (string, error) {
	out, err := g.run("api", "user", "--jq", ".login")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (g ghCLI) reviews(pr int) ([]review, error) {
	var reviews []review
	err := g.getJSON(fmt.Sprintf("repos/{owner}/{repo}/pulls/%d/reviews", pr), &reviews)
	return reviews, err
}

func (g ghCLI) reviewedBy(pr int, author, sha string) (bool, error) {
	reviews, err := g.reviews(pr)
	if err != nil {
		return false, err
	}
	for _, r := range reviews {
		if r.User.Login == author && r.CommitID == sha {
			return true, nil
		}
	}
	return false, nil
}

// requestReview summons a reviewer onto the PR. The login is used verbatim:
// for Copilot the request wants "copilot-pull-request-reviewer[bot]" — the
// same login its reviews carry, so one --await-review value serves both
// (verified live on wixzettle #38, 2026-07-17; the pending-request list
// echoes it back as "Copilot", a third spelling this code never needs).
func (g ghCLI) requestReview(pr int, reviewer string) error {
	_, err := g.run("api", "-X", "POST",
		fmt.Sprintf("repos/{owner}/{repo}/pulls/%d/requested_reviewers", pr),
		"-f", "reviewers[]="+reviewer)
	return err
}

// runLog fetches a workflow run's full log — the haystack the flake marker
// is looked for in. Big (whole-run output), but read at most a handful of
// times per watch and only while a run is failing.
func (g ghCLI) runLog(runID string) (string, error) {
	out, err := g.run("run", "view", runID, "--log")
	return string(out), err
}

// rerunFailed reruns only a run's failed jobs, like the Actions UI button.
// Idempotent in the way that matters here: rerunning a run that is already
// rerunning is an error answer from GitHub, not a second rerun.
func (g ghCLI) rerunFailed(runID string) error {
	_, err := g.run("run", "rerun", runID, "--failed")
	return err
}

// getJSON reads one API page of up to 100 items — plenty for this repo's
// PRs, and it sidesteps --paginate's output shape (concatenated JSON
// documents, which a single Unmarshal cannot read).
func (g ghCLI) getJSON(path string, v any) error {
	out, err := g.run("api", path+"?per_page=100")
	if err != nil {
		return err
	}
	return json.Unmarshal(out, v)
}

// transientDelays is the retry schedule for a gh call worth attempting
// again. It has to outlast a laptop waking up: closing the lid kills
// the in-flight socket, and the network takes a few seconds to come back
// once the process resumes (observed live 2026-07-22 — a closed lid ended
// the watch on PR #68 with "read tcp …: read: connection reset by peer",
// seconds after it had reported ready and was only waiting for the merge).
// The sleep itself costs no attempts: the process is suspended alongside
// the machine, so the whole schedule is spent on the wake, not on the night.
var transientDelays = []time.Duration{
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
	20 * time.Second,
	40 * time.Second,
}

// retry calls fn until it succeeds, fails in a way that will not fix itself,
// or runs out of delays — len(delays)+1 attempts in all. Each retry is
// announced through note: a watch that silently stalls for a minute looks
// exactly like a hung one, and the whole point of the log is that the wait
// is legible.
func retry[T any](
	fn func() (T, error),
	delays []time.Duration,
	sleep func(time.Duration),
	note func(err error, next time.Duration),
) (T, error) {
	for _, d := range delays {
		v, err := fn()
		if err == nil || !transient(err) {
			return v, err
		}

		note(err, d)
		sleep(d)
	}
	return fn()
}

// transientPatterns are the failures worth another attempt, matched against
// gh's own error text. Two families qualify.
//
// The call never reached GitHub: the Go HTTP client's transport errors,
// which is what a sleeping or waking laptop produces. Or GitHub answered
// with a status it calls temporary itself (observed live 2026-07-20 — a 502
// mid-poll ended the watch on PR #49).
//
// 4xx is deliberately absent. Those are answers, not accidents: a 404 on a
// deleted PR or a 401 on an expired token will read exactly the same in
// forty seconds, and retrying them would turn a clear failure into a stall.
var transientPatterns = []string{
	// No answer came back.
	"dial tcp",
	"connection reset",
	"connection refused",
	"network is unreachable",
	"no route to host",
	"no such host",
	"i/o timeout",
	"operation timed out",
	"TLS handshake",
	"context deadline exceeded",
	"unexpected EOF",

	// An answer came back, saying "later".
	"HTTP 502",
	"HTTP 503",
	"HTTP 504",
}

// transient reports whether a gh failure is worth another attempt. Positive
// match only — an unrecognised error is treated as terminal, so a genuinely
// broken watch still ends promptly instead of retrying into silence.
func transient(err error) bool {
	if err == nil {
		return false
	}

	msg := err.Error()
	for _, p := range transientPatterns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// flatten bounds a body for a one-line log record: newlines become " | "
// and the result is capped by runes, not bytes.
func flatten(s string) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == '\r' || r == '\n' }), " | ")
	runes := []rune(s)
	if len(runes) > 400 {
		return string(runes[:400])
	}
	return s
}
