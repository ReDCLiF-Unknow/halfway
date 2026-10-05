// Command halfway is a command-line client for a Halfway server's JSON API.
package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/go-resty/resty/v2"
	"github.com/spf13/cobra"
)

type apiError struct {
	Error string `json:"error"`
}

// The shapes the API answers with, as much of them as the CLI shows.
type (
	user struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Token string `json:"token"`
	}
	summary struct {
		ID       int64  `json:"id"`
		Title    string `json:"title"`
		Status   string `json:"status"`
		Deadline string `json:"deadline"`
		People   int    `json:"people"`
		Answered int    `json:"answered"`
		Mine     bool   `json:"answered_by_you"`
		Chosen   string `json:"chosen"`
		Venue    string `json:"venue"`
	}
	vote struct {
		Name   string `json:"name"`
		Answer int    `json:"answer"`
	}
	pollTime struct {
		Number  int    `json:"number"`
		Label   string `json:"label"`
		CanCome int    `json:"can_come"`
		Mine    string `json:"your_answer"`
		Leading bool   `json:"leading"`
		Chosen  bool   `json:"chosen"`
		Votes   []vote `json:"votes"`
	}
	pollView struct {
		Poll struct {
			ID       int64  `json:"id"`
			Title    string `json:"title"`
			Category string `json:"category"`
			Status   string `json:"status"`
			Quorum   int    `json:"quorum"`
		} `json:"poll"`
		Times         []pollTime              `json:"times"`
		People        []struct{ Name string } `json:"people"`
		Organizer     bool                    `json:"organizer"`
		InviteURL     string                  `json:"invite_url"`
		OrganizerURL  string                  `json:"organizer_url"`
		DeadlineLabel string                  `json:"deadline_label"`
		Venue         *struct {
			Name, Address string
		} `json:"venue"`
	}
)

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseID(s string) (string, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return "", fmt.Errorf("%q is not a poll number (see: halfway polls)", s)
	}
	return strconv.FormatInt(n, 10), nil
}

func parseNumber(s, what string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a %s number (see: halfway show)", s, what)
	}
	return n, nil
}

// newRoot builds the command tree. It is separate from main so that tests can
// run commands against a test server and read what they print.
func newRoot() *cobra.Command {
	var server, token string
	client := resty.New()

	root := &cobra.Command{
		Use:           "halfway",
		Short:         "Plan meetups with Halfway from the terminal",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(*cobra.Command, []string) {
			client.SetBaseURL(strings.TrimRight(server, "/")).SetError(&apiError{})
			if token != "" {
				client.SetAuthToken(token)
			}
		},
	}
	root.PersistentFlags().StringVarP(&server, "server", "s", envOr("HALFWAY_SERVER", "http://localhost:8080"), "server base URL (env HALFWAY_SERVER)")
	root.PersistentFlags().StringVarP(&token, "token", "t", os.Getenv("HALFWAY_TOKEN"), "your sign-in key (env HALFWAY_TOKEN), from your profile or `halfway register`")

	// check turns a resty response into a Go error.
	check := func(resp *resty.Response, err error) error {
		if err != nil {
			return err
		}
		if resp.IsError() {
			if e, ok := resp.Error().(*apiError); ok && e.Error != "" {
				return fmt.Errorf("%s: %s", resp.Status(), e.Error)
			}
			return fmt.Errorf("%s", resp.Status())
		}
		return nil
	}

	register := &cobra.Command{Use: "register NAME", Short: "Pick a name; prints your sign-in key", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var u user
			if err := check(client.R().SetBody(map[string]string{"name": args[0]}).SetResult(&u).Post("/api/users")); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Hello, %s. Your sign-in key:\n\n  %s\n\n", u.Name, u.Token)
			fmt.Fprintln(out, "Keep it safe: it is the only way back in. Use it with -t, or:")
			fmt.Fprintf(out, "  export HALFWAY_TOKEN=%s        # PowerShell: $env:HALFWAY_TOKEN = \"%s\"\n", u.Token, u.Token)
			return nil
		}}

	whoami := &cobra.Command{Use: "whoami", Short: "Show who you are signed in as", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var u user
			if err := check(client.R().SetResult(&u).Get("/api/me")); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s (id %d)\n", u.Name, u.ID)
			return nil
		}}

	polls := &cobra.Command{Use: "polls", Short: "Show every poll you are on", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var ps []summary
			if err := check(client.R().SetResult(&ps).Get("/api/polls")); err != nil {
				return err
			}
			if len(ps) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), `No polls yet. Make one with: halfway new "Friday dinner" --time "2026-10-09 19:30"`)
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tTITLE\tSTATUS\tANSWERED")
			for _, p := range ps {
				status := "open, decides " + strings.Replace(p.Deadline, "T", " ", 1)
				switch p.Status {
				case "confirmed":
					status = "on " + strings.Replace(p.Chosen, "T", " ", 1)
					if p.Venue != "" {
						status += " at " + p.Venue
					}
				case "cancelled":
					status = "called off"
				}
				answered := fmt.Sprintf("%d of %d", p.Answered, p.People)
				if p.Status == "open" && !p.Mine {
					answered += " (not you yet)"
				}
				fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", p.ID, p.Title, status, answered)
			}
			return w.Flush()
		}}

	var times []string
	var kind, deadline string
	var minimum int
	var places bool
	newPoll := &cobra.Command{Use: "new TITLE", Short: "Make a poll", Args: cobra.ExactArgs(1),
		Example: `  halfway new "Friday dinner" --time "2026-10-09 19:30" --time "2026-10-10 19:30" --kind dinner --min 4`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(times) == 0 {
				return fmt.Errorf(`give at least one --time, like --time "2026-10-09 19:30"`)
			}
			var v pollView
			body := map[string]any{"title": args[0], "category": kind, "times": times, "deadline": deadline, "quorum": minimum, "places": places}
			if err := check(client.R().SetBody(body).SetResult(&v).Post("/api/polls")); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Made poll %d: %s, deciding %s.\n", v.Poll.ID, v.Poll.Title, v.DeadlineLabel)
			fmt.Fprintf(out, "Send everyone this link:\n\n  %s\n", v.InviteURL)
			return nil
		}}
	newPoll.Flags().StringArrayVar(&times, "time", nil, `a time it could be, "YYYY-MM-DD HH:MM" (up to 8)`)
	newPoll.Flags().StringVar(&kind, "kind", "other", "coffee, dinner, drinks, hike or other")
	newPoll.Flags().StringVar(&deadline, "deadline", "", `when it decides, "YYYY-MM-DD HH:MM" (default: a day before the first time)`)
	newPoll.Flags().IntVar(&minimum, "min", 0, "only on if at least this many can come")
	newPoll.Flags().BoolVar(&places, "places", false, "also find a place halfway between everyone (if the server has it on)")

	show := &cobra.Command{Use: "show POLL", Short: "Show a poll: its times, who can come, and your answers", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			var v pollView
			if err := check(client.R().SetResult(&v).Get("/api/polls/" + id)); err != nil {
				return err
			}
			printPoll(cmd.OutOrStdout(), v)
			return nil
		}}

	answer := &cobra.Command{Use: "answer POLL TIME yes|maybe|no", Short: "Say whether you can make one of a poll's times", Args: cobra.ExactArgs(3),
		Long:    "Say whether you can make one of a poll's times. TIME is its number in `halfway show`; maybe means if needed.",
		Example: "  halfway answer 3 2 yes\n  halfway answer 3 1 maybe",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			n, err := parseNumber(args[1], "time")
			if err != nil {
				return err
			}
			var v pollView
			if err := check(client.R().SetBody(map[string]any{"time": n, "answer": args[2]}).SetResult(&v).Post("/api/polls/" + id + "/answers")); err != nil {
				return err
			}
			printPoll(cmd.OutOrStdout(), v)
			return nil
		}}

	join := &cobra.Command{Use: "join LINK", Short: "Join a poll by its invite (or organizer) link", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var v pollView
			if err := check(client.R().SetBody(map[string]string{"link": args[0]}).SetResult(&v).Post("/api/join")); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Joined poll %d: %s\n\n", v.Poll.ID, v.Poll.Title)
			printPoll(cmd.OutOrStdout(), v)
			return nil
		}}

	decide := &cobra.Command{Use: "decide POLL", Short: "Decide a poll now, as if its deadline had passed (organizers)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			var v pollView
			if err := check(client.R().SetResult(&v).Post("/api/polls/" + id + "/decide")); err != nil {
				return err
			}
			printPoll(cmd.OutOrStdout(), v)
			return nil
		}}

	pick := &cobra.Command{Use: "pick POLL TIME", Short: "Make it one of the times, whatever the answers say (organizers)", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			n, err := parseNumber(args[1], "time")
			if err != nil {
				return err
			}
			var v pollView
			if err := check(client.R().SetBody(map[string]int{"time": n}).SetResult(&v).Post("/api/polls/" + id + "/pick")); err != nil {
				return err
			}
			printPoll(cmd.OutOrStdout(), v)
			return nil
		}}

	leave := &cobra.Command{Use: "leave POLL", Short: "Leave a poll, and take your answers with you", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			if err := check(client.R().Post("/api/polls/" + id + "/leave")); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Left poll %s.\n", id)
			return nil
		}}

	rm := &cobra.Command{Use: "rm POLL", Short: "Delete a poll for everyone on it (organizers)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			if err := check(client.R().Delete("/api/polls/" + id)); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted poll %s. It can be brought back for a day: halfway restore %s\n", id, id)
			return nil
		}}

	restore := &cobra.Command{Use: "restore POLL", Short: "Undo deleting a poll, within a day", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			var v pollView
			if err := check(client.R().SetResult(&v).Post("/api/polls/" + id + "/restore")); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Restored poll %d: %s\n", v.Poll.ID, v.Poll.Title)
			return nil
		}}

	var organizerLink bool
	invite := &cobra.Command{Use: "invite POLL", Short: "Print a poll's invite link", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			var v pollView
			if err := check(client.R().SetResult(&v).Get("/api/polls/" + id)); err != nil {
				return err
			}
			if !organizerLink {
				fmt.Fprintln(cmd.OutOrStdout(), v.InviteURL)
				return nil
			}
			if v.OrganizerURL == "" {
				return fmt.Errorf("only the poll's organizers have its organizer link")
			}
			fmt.Fprintln(cmd.OutOrStdout(), v.OrganizerURL)
			return nil
		}}
	invite.Flags().BoolVar(&organizerLink, "organizer", false, "the organizer link instead, which makes whoever joins with it an organizer")

	root.AddCommand(register, whoami, polls, newPoll, show, answer, join, decide, pick, leave, rm, restore, invite)
	return root
}

// printPoll shows a poll the way a person reads it: where it stands, then
// each time with who can come and what you said.
func printPoll(out io.Writer, v pollView) {
	fmt.Fprintf(out, "%s (poll %d)\n", v.Poll.Title, v.Poll.ID)
	switch v.Poll.Status {
	case "confirmed":
		for _, t := range v.Times {
			if t.Chosen {
				fmt.Fprintf(out, "It's on: %s", t.Label)
			}
		}
		if v.Venue != nil {
			fmt.Fprintf(out, " at %s", v.Venue.Name)
			if v.Venue.Address != "" {
				fmt.Fprintf(out, ", %s", v.Venue.Address)
			}
		}
		fmt.Fprintln(out)
	case "cancelled":
		fmt.Fprintln(out, "Called off.")
	default:
		line := "Open: decides " + v.DeadlineLabel
		if v.Poll.Quorum > 0 {
			line += fmt.Sprintf(", or as soon as %d can come", v.Poll.Quorum)
		}
		fmt.Fprintln(out, line+".")
	}
	fmt.Fprintln(out)
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "#\tTIME\tCAN COME\tYOU\tWHO")
	for _, t := range v.Times {
		mark := ""
		if t.Leading {
			mark = "  <- leading"
		}
		if t.Chosen {
			mark = "  <- chosen"
		}
		var who []string
		for _, vt := range t.Votes {
			switch vt.Answer {
			case 2:
				who = append(who, vt.Name)
			case 1:
				who = append(who, vt.Name+" (if needed)")
			}
		}
		mine := t.Mine
		if mine == "" {
			mine = "-"
		}
		fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%s%s\n", t.Number, t.Label, t.CanCome, mine, strings.Join(who, ", "), mark)
	}
	w.Flush()
	fmt.Fprintf(out, "\nInvite link: %s\n", v.InviteURL)
}
