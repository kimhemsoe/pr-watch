# pr-watch

Two small commands for babysitting GitHub pull requests from a terminal (or an
agent), built on the [`gh` CLI](https://cli.github.com/). No tokens, no
configuration: auth, host and owner/repo resolution stay `gh`'s problem, so
they work in any checkout `gh` works in.

```
go install github.com/kimhemsoe/pr-watch/cmd/...@latest
```

## pr-watch

Follows a pull request until it *settles*: all checks completed **and** no new
review activity for a grace period. Along the way it prints new inline
comments, discussion comments and review summaries as they arrive (inline
comment ids included, for reply automation with `pr-reply`).

```
pr-watch [pr] [flags]
```

With no `pr`, the current branch's PR is watched.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--interval s` | 30 | poll interval |
| `--grace s` | 240 | quiet period before settling — long enough to catch bot reviews that land after CI |
| `--await-review AUTHOR` | | hold until this author has reviewed the **current head**; a review can land minutes after CI with no earlier visible signal, so quiet alone proves nothing |
| `--await-timeout s` | 1800 | give up on the awaited reviewer |
| `--request-review` | | summon the `--await-review` reviewer on every new head; reviewers like Copilot never review unprompted (use the login its reviews carry, e.g. `copilot-pull-request-reviewer[bot]`) |
| `--until-merged` | | after settling green, keep watching and exit on the merge — post-merge automation can hook the process exit |
| `--rerun-flake MARKER` | | rerun a run's failed jobs when its log contains this substring — a known CI flake's signature |
| `--max-reruns n` | 2 | total flake reruns to attempt in one watch |

Exit code: `0` = settled all-green (and merged, with `--until-merged`),
`1` = failing checks or closed unmerged, `2` = usage.

Behaviour worth knowing:

- **A push mid-watch re-pins automatically** — checks, awaited review and
  grace all restart against the new head, and `--request-review` summons the
  reviewer again (every push invalidates the previous review).
- **Checks must register on the current head before they are trusted**: right
  after a push, GitHub reports the *previous* commit's completed checks for a
  while, and watching too early would "settle" against stale state.
- **Your own activity is filtered out** — answering review threads while the
  watch runs is not news, and it must not reset the settle grace, or a PR
  being replied to never settles.
- **Transient failures are retried**, on a schedule that outlasts a laptop
  waking up from sleep. Failures that will read the same later (404, expired
  token) end the watch at once.
- **Never pipe the output** (`| tail` and friends buffer the live feed into
  invisibility). Run it unpiped, or redirect to a file and tail that.

Typical invocations:

```sh
# watch the current branch's PR
pr-watch

# arm-and-forget: summon Copilot on every push, exit on the merge
pr-watch 29 --await-review "copilot-pull-request-reviewer[bot]" --request-review --until-merged

# auto-rerun a known infra flake, at most twice
pr-watch --rerun-flake 'Failed to initialize container localstack'
```

## pr-reply

Posts a threaded reply to a PR review comment. Replying by hand is a raw
`gh api` call whose body must first become valid JSON — quoting markdown
inline breaks on a stray backtick, a newline, a bare `true` that gh coerces
to a boolean. `pr-reply` reads the body from a file (or stdin) and marshals
it, so any markdown survives intact.

```
pr-reply <pr> <comment-id> <body-file>   ("-" reads stdin)
```

`<comment-id>` is the review comment's numeric id — the one `pr-watch`
prints as `id:`. Replying to any comment in a thread threads the reply
correctly. The POST is attempted exactly once, never retried: a retry after
a dropped connection could double-post a reply that had in fact landed.

```sh
pr-reply 29 987654321 - <<'EOF'
Fixed in abc123 — the check now runs before the cast.
EOF
```
