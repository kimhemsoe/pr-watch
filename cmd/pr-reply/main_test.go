package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRequestBuildsThreadedReplyCall(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "body.md")
	// The exact shapes that break inline quoting: backticks, newlines, a
	// bare `true` that gh api -f would coerce to a boolean, and quotes.
	body := "Fixed in `abc123`.\n\nThe flag is now true — see \"notes\".\n"
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	path, payload, err := request([]string{"29", "987654", file}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if path != "repos/{owner}/{repo}/pulls/29/comments/987654/replies" {
		t.Fatalf("path = %q", path)
	}

	var got struct{ Body string }
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("payload is not valid JSON: %v\n%s", err, payload)
	}
	if got.Body != body {
		t.Fatalf("body did not survive the round trip:\n%q\n%q", got.Body, body)
	}
}

func TestRequestReadsStdinOnDash(t *testing.T) {
	_, payload, err := request([]string{"1", "2", "-"}, strings.NewReader("from stdin"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Body string }
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	if got.Body != "from stdin" {
		t.Fatalf("body = %q", got.Body)
	}
}

func TestRequestRejectsBadArguments(t *testing.T) {
	cases := map[string][]string{
		"too few args":      {"1", "2"},
		"non-numeric pr":    {"abc", "2", "-"},
		"non-numeric id":    {"1", "abc", "-"},
		"missing body file": {"1", "2", filepath.Join(t.TempDir(), "absent.md")},
	}
	for name, args := range cases {
		if _, _, err := request(args, strings.NewReader("")); err == nil {
			t.Errorf("%s: accepted %v", name, args)
		}
	}
}

// The point of summarising: gh's answer echoes the reply body, the diff hunk
// and the author's URLs back at a caller that is usually an agent paying for
// every token of it.
func TestPostedKeepsOnlyTheURL(t *testing.T) {
	response := []byte(`{
		"id": 987654,
		"html_url": "https://github.com/o/r/pull/29#discussion_r987654",
		"diff_hunk": "@@ -1,3 +1,4 @@\n-old\n+new",
		"body": "Fixed in abc123.",
		"user": {"login": "khr", "avatar_url": "https://example.invalid/a.png"},
		"reactions": {"total_count": 0, "+1": 0}
	}`)

	got := posted(response)
	if want := "pr-reply: posted https://github.com/o/r/pull/29#discussion_r987654"; got != want {
		t.Fatalf("posted() = %q, want %q", got, want)
	}
	for _, echoed := range []string{"diff_hunk", "Fixed in abc123", "avatar_url", "reactions"} {
		if strings.Contains(got, echoed) {
			t.Errorf("posted() echoed %q back: %s", echoed, got)
		}
	}
}

// A response gh never promised (an API change, a proxy's HTML) still means the
// reply landed — say so rather than dumping the surprise into the context.
func TestPostedToleratesAnUnexpectedResponse(t *testing.T) {
	for name, response := range map[string]string{
		"not JSON":       "<html>502 Bad Gateway</html>",
		"empty":          "",
		"no html_url":    `{"id": 1}`,
		"null html_url":  `{"html_url": null}`,
		"empty html_url": `{"html_url": ""}`,
	} {
		if got := posted([]byte(response)); got != "pr-reply: posted" {
			t.Errorf("%s: posted() = %q", name, got)
		}
	}
}

// An empty body is a mistake (a typoed filename redirect, an empty heredoc),
// and GitHub would post it as a blank reply — reject it before the API call.
func TestRequestRejectsEmptyBody(t *testing.T) {
	if _, _, err := request([]string{"1", "2", "-"}, strings.NewReader("  \n")); err == nil {
		t.Error("accepted a whitespace-only body")
	}
}
