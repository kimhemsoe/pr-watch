// pr-reply posts a threaded reply to a PR review comment — the
// review-conversation seam.
//
// Replying by hand is a raw `gh api` call whose body must first become
// valid JSON, and quoting markdown inline breaks (a stray backtick, a
// newline, a bare `true` that gh coerces to a boolean). This reads the
// body from a file — or stdin — and marshals it, so any markdown survives
// intact. gh fills in {owner}/{repo} from the checkout's remote.
//
// Usage: pr-reply <pr> <comment-id> <body-file>   ("-" reads stdin)
//
//	<comment-id> is the review comment's numeric id; replying to any
//	comment in a thread threads the reply correctly.
//
// The POST is attempted exactly once, never retried: a retry after a
// dropped connection could double-post a reply that had in fact landed.
//
// Success prints one line — the new comment's URL. gh's own answer is the
// whole comment object, which echoes the reply back at whoever wrote it;
// see posted.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
)

func main() {
	path, payload, err := request(os.Args[1:], os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pr-reply:", err)
		fmt.Fprintln(os.Stderr, `usage: pr-reply <pr> <comment-id> <body-file>  ("-" reads stdin)`)
		os.Exit(2)
	}

	var response bytes.Buffer
	cmd := exec.Command("gh", "api", "--method", "POST", path, "--input", "-")
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = &response
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// gh diagnoses the failure on stderr but leaves the API's own error
		// body on stdout, where the detail is — relay it.
		if body := bytes.TrimRight(response.Bytes(), "\n"); len(body) > 0 {
			fmt.Fprintf(os.Stderr, "%s\n", body)
		}
		os.Exit(1)
	}
	fmt.Println(posted(response.Bytes()))
}

// posted summarises the created reply. gh answers a successful POST with the
// whole comment object — around 3 KB of JSON, of which the caller's own body
// is echoed back and the rest is the diff hunk, two dozen author URLs and the
// reaction counts. The caller is usually an agent, so that echo lands in a
// context window as roughly a thousand tokens saying nothing it did not just
// write; only the new comment's URL is worth keeping.
func posted(response []byte) string {
	var reply struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(response, &reply); err != nil || reply.HTMLURL == "" {
		return "pr-reply: posted"
	}
	return "pr-reply: posted " + reply.HTMLURL
}

// request turns the arguments into the API path and JSON payload of the
// reply call. Pure except for reading the body, so the argument contract
// and the escaping are testable without a GitHub.
func request(args []string, stdin io.Reader) (string, []byte, error) {
	if len(args) != 3 {
		return "", nil, fmt.Errorf("want 3 arguments, got %d", len(args))
	}

	pr, err := strconv.Atoi(args[0])
	if err != nil {
		return "", nil, fmt.Errorf("<pr> must be a number, got %q", args[0])
	}
	commentID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return "", nil, fmt.Errorf("<comment-id> must be the review comment's numeric id, got %q", args[1])
	}

	var body []byte
	if args[2] == "-" {
		body, err = io.ReadAll(stdin)
	} else {
		body, err = os.ReadFile(args[2])
	}
	if err != nil {
		return "", nil, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return "", nil, fmt.Errorf("the reply body is empty")
	}

	payload, err := json.Marshal(map[string]string{"body": string(body)})
	if err != nil {
		return "", nil, err
	}

	path := fmt.Sprintf("repos/{owner}/{repo}/pulls/%d/comments/%d/replies", pr, commentID)
	return path, payload, nil
}
