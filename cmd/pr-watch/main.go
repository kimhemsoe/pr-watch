// pr-watch follows a pull request until it settles: checks completed, no
// new review activity for a grace period, and (with --await-review) the
// named reviewer has reviewed the current head — review can land minutes
// after CI with no earlier visible signal, so quiet alone proves nothing.
// --request-review summons the awaited reviewer on every new head (watch
// start and each push): reviewers like Copilot never review unprompted,
// so without a request the await would only ever time out. A push
// mid-watch re-pins to the new head automatically. With --until-merged it
// keeps watching after "ready" and exits on the merge, so post-merge
// automation can hook the process exit.
//
// --rerun-flake recognizes a known CI flake: when a failing run's log
// contains the given marker (a plain substring), its failed jobs are rerun
// — at most --max-reruns times per watch — instead of the watch settling
// red. Only a triggered rerun keeps the watch waiting; a failure whose log
// lacks the marker settles as the failure it is.
//
// A watch is meant to be armed and left alone, so a gh call that fails for
// a reason that will not still be true in a minute — a slept laptop's dead
// socket, a 502 — is retried rather than ending the watch. Failures that
// will read the same later (404, expired token) still exit at once.
//
// Usage: pr-watch [pr] [--interval s] [--grace s]
//
//	[--await-review AUTHOR] [--await-timeout s] [--request-review]
//	[--until-merged] [--rerun-flake MARKER] [--max-reruns n]
//
// Defaults: the current branch's PR, 30s interval, 240s grace, 1800s
// await-timeout, 2 max-reruns. Exit: 0 = all-green (and merged, with
// --until-merged), 1 = failing checks or closed unmerged.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"
)

func main() {
	args := os.Args[1:]

	// The PR number rides in front by convention (pr-watch 29 --grace 180);
	// flag parsing wants flags first, so pop it before parsing.
	pr := 0
	if len(args) > 0 {
		if n, err := strconv.Atoi(args[0]); err == nil {
			pr = n
			args = args[1:]
		}
	}

	fs := flag.NewFlagSet("pr-watch", flag.ExitOnError)
	interval := fs.Int("interval", 30, "poll interval in seconds")
	grace := fs.Int("grace", 240, "quiet period before settling, in seconds")
	awaitReviewer := fs.String("await-review", "", "hold until this author has reviewed the current head")
	awaitTimeout := fs.Int("await-timeout", 1800, "give up waiting for the reviewer after this many seconds")
	requestReview := fs.Bool("request-review", false, "request the --await-review reviewer on every new head")
	untilMerged := fs.Bool("until-merged", false, "after settling, keep watching until the PR is merged or closed")
	rerunFlake := fs.String("rerun-flake", "", "rerun failed jobs when their run log contains this substring (a known flake marker)")
	maxReruns := fs.Int("max-reruns", 2, "total --rerun-flake reruns to attempt in one watch")
	// ExitOnError: a parse failure prints usage and exits 2 by itself.
	_ = fs.Parse(args)
	// A non-positive interval would make every sleep return immediately
	// and spin the poll loop against the API.
	if *interval <= 0 || *grace < 0 || *awaitTimeout < 0 || *maxReruns < 0 {
		fmt.Fprintln(os.Stderr, "pr-watch: --interval must be positive; --grace, --await-timeout and --max-reruns must not be negative")
		os.Exit(2)
	}
	if *requestReview && *awaitReviewer == "" {
		fmt.Fprintln(os.Stderr, "pr-watch: --request-review needs --await-review to know who to summon")
		os.Exit(2)
	}

	gh := newGHCLI(os.Stdout, time.Now)
	if pr == 0 {
		n, err := gh.currentPR()
		if err != nil {
			fmt.Fprintln(os.Stderr, "pr-watch: no PR given and none found for the current branch:", err)
			os.Exit(2)
		}
		pr = n
	}

	w := &watcher{
		gh: gh,
		cfg: config{
			pr:            pr,
			interval:      time.Duration(*interval) * time.Second,
			grace:         time.Duration(*grace) * time.Second,
			awaitReviewer: *awaitReviewer,
			awaitTimeout:  time.Duration(*awaitTimeout) * time.Second,
			requestReview: *requestReview,
			untilMerged:   *untilMerged,
			rerunFlake:    *rerunFlake,
			maxReruns:     *maxReruns,
		},
		out:   os.Stdout,
		now:   time.Now,
		sleep: time.Sleep,
	}

	code, err := w.run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "pr-watch:", err)
		os.Exit(1)
	}
	os.Exit(code)
}
