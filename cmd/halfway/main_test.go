package main

import (
	"bytes"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"halfway/internal/store"
	"halfway/internal/web"
)

// The CLI is a documented way to use Halfway, so it is tested the way someone
// uses it: run a command against a real server and read what it printed.

type cli struct {
	t     *testing.T
	url   string
	token string
}

func newCLI(t *testing.T) *cli {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "cli.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(web.New(s, nil, nil))
	t.Cleanup(func() { srv.Close(); s.Close() })
	return &cli{t: t, url: srv.URL}
}

// run executes a command and returns what it wrote. It fails the test if the
// command did.
func (c *cli) run(args ...string) string {
	c.t.Helper()
	out, err := c.try(args...)
	if err != nil {
		c.t.Fatalf("halfway %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// try is run for commands that are expected to fail.
func (c *cli) try(args ...string) (string, error) {
	c.t.Helper()
	full := append([]string{"-s", c.url}, args...)
	if c.token != "" {
		full = append([]string{"-t", c.token}, full...)
	}
	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(full)
	err := root.Execute()
	return out.String(), err
}

// signIn registers somebody and keeps their key for later commands.
func (c *cli) signIn(name string) string {
	c.t.Helper()
	out := c.run("register", name)
	m := regexp.MustCompile(`\b([0-9a-f]{64})\b`).FindStringSubmatch(out)
	if m == nil {
		c.t.Fatalf("register printed no key:\n%s", out)
	}
	c.token = m[1]
	return m[1]
}

func contains(t *testing.T, what, out, want string) {
	t.Helper()
	if !strings.Contains(out, want) {
		t.Errorf("%s: expected %q in:\n%s", what, want, out)
	}
}

func day(n int, clock string) string {
	return time.Now().AddDate(0, 0, n).Format("2006-01-02") + " " + clock
}

func TestPlanningFromTheTerminal(t *testing.T) {
	c := newCLI(t)
	if _, err := c.try("polls"); err == nil || !strings.Contains(err.Error(), "halfway register") {
		t.Errorf("without a key, polls should say how to get one: %v", err)
	}
	anna := c.signIn("Anna")
	contains(t, "whoami", c.run("whoami"), "Anna (id 1)")
	contains(t, "no polls", c.run("polls"), "No polls yet")

	if _, err := c.try("new", "Nothing to choose"); err == nil {
		t.Error("a poll with no times was made")
	}
	if _, err := c.try("new", "Too late", "--time", day(-1, "19:00")); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("a time gone by: %v", err)
	}
	out := c.run("new", "Friday dinner", "--kind", "dinner", "--min", "2",
		"--time", day(4, "19:30"), "--time", day(3, "19:30"), "--deadline", day(2, "18:00"))
	contains(t, "new", out, "Made poll 1: Friday dinner")
	link := regexp.MustCompile(`http://\S+/i/\S+`).FindString(out)
	if link == "" {
		t.Fatalf("new printed no invite link:\n%s", out)
	}
	contains(t, "invite", c.run("invite", "1"), link)

	out = c.run("show", "1")
	contains(t, "show", out, "Open: decides")
	contains(t, "show", out, "or as soon as 2 can come")
	contains(t, "show", out, "1  "+store.Label(strings.Replace(day(3, "19:30"), " ", "T", 1)))

	// Ben joins by the link and answers; the minimum decides it.
	c.signIn("Ben")
	out = c.run("join", link)
	contains(t, "join", out, "Joined poll 1: Friday dinner")
	if _, err := c.try("decide", "1"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("Ben deciding: %v", err)
	}
	if _, err := c.try("answer", "1", "9", "yes"); err == nil {
		t.Error("answering a time that does not exist")
	}
	if _, err := c.try("answer", "1", "1", "perhaps"); err == nil {
		t.Error("answering perhaps")
	}
	out = c.run("answer", "1", "2", "maybe")
	contains(t, "Ben's answer", out, "if-needed")
	c.token = anna
	out = c.run("answer", "1", "2", "yes")
	contains(t, "deciding answer", out, "It's on: "+store.Label(strings.Replace(day(4, "19:30"), " ", "T", 1)))
	contains(t, "deciding answer", out, "<- chosen")
	contains(t, "polls after", c.run("polls"), "on "+day(4, "19:30"))

	// The organizer overrules it.
	out = c.run("pick", "1", "1")
	contains(t, "pick", out, "It's on: "+store.Label(strings.Replace(day(3, "19:30"), " ", "T", 1)))

	// Deleting, and taking it back.
	contains(t, "rm", c.run("rm", "1"), "halfway restore 1")
	if _, err := c.try("show", "1"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("showing a deleted poll: %v", err)
	}
	contains(t, "restore", c.run("restore", "1"), "Restored poll 1: Friday dinner")
}

func TestLeavingAndJoiningByOrganizerLink(t *testing.T) {
	c := newCLI(t)
	anna := c.signIn("Anna")
	c.run("new", "Coffee", "--time", day(2, "10:00"))
	c.signIn("Ben")
	if _, err := c.try("join", "http://example/i/not-a-code"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("joining with a made-up link: %v", err)
	}
	if _, err := c.try("show", "1"); err == nil {
		t.Error("Ben saw a poll he is not on")
	}
	ben := c.token
	if _, err := c.try("invite", "1"); err == nil {
		t.Error("Ben got the invite link of a poll he is not on")
	}
	c.token = anna
	org := strings.TrimSpace(c.run("invite", "1", "--organizer"))
	if !strings.Contains(org, "/o/") {
		t.Fatalf("organizer link %q", org)
	}
	// Joining with the organizer link makes Ben an organizer, who can decide.
	c.token = ben
	contains(t, "join as organizer", c.run("join", org), "Joined poll 1: Coffee")
	if _, err := c.try("invite", "1", "--organizer"); err != nil {
		t.Errorf("Ben, now an organizer, has no organizer link: %v", err)
	}
	contains(t, "Ben deciding", c.run("decide", "1"), "Called off.")
	// With a second organizer, Anna may go.
	c.token = anna
	contains(t, "leave", c.run("leave", "1"), "Left poll 1.")
	if _, err := c.try("show", "1"); err == nil {
		t.Error("Anna still sees the poll she left")
	}
	if _, err := c.try("show", "x"); err == nil || !strings.Contains(err.Error(), "not a poll number") {
		t.Errorf("a poll that is not a number: %v", err)
	}
}
