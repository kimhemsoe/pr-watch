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

// An empty body is a mistake (a typoed filename redirect, an empty heredoc),
// and GitHub would post it as a blank reply — reject it before the API call.
func TestRequestRejectsEmptyBody(t *testing.T) {
	if _, _, err := request([]string{"1", "2", "-"}, strings.NewReader("  \n")); err == nil {
		t.Error("accepted a whitespace-only body")
	}
}
