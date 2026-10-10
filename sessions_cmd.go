// Session-management commands. They round out the CLI on their own, and they are
// also the API that the recall-app desktop client is built on
// (https://github.com/ramsrib/recall-app):
//
//	recall list                 one row per session (+ pin/summary state)
//	recall pin / unpin <id>     manage pinned sessions
//	recall summary <id>         concise per-session overview (via codex), cached
//	recall resume <id>          emit the resume command + cwd for a session
//
// Two of these reach outside the process, and both are deliberate:
//
//   - `summary` pipes transcript text into `codex exec` (network-backed). It is the
//     only command in recall that sends conversation content off the machine.
//   - `resume` PRINTS a resume command that carries --dangerously-skip-permissions
//     (Claude) or --dangerously-bypass-approvals-and-sandbox (Codex). It never
//     executes it; running it is the caller's explicit choice.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ramsrib/recall-cli/internal/adapter"
	"github.com/ramsrib/recall-cli/internal/index"
	"github.com/ramsrib/recall-cli/internal/store"
)

// --- recall list ------------------------------------------------------------

func runList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	source := fs.String("source", "", "claude|codex")
	project := fs.String("project", "", "substring match on project path")
	since := fs.String("since", "", "only sessions on/after this date (YYYY-MM-DD)")
	limit := fs.Int("limit", 0, "max sessions (0 = all)")
	mode := fs.String("mode", "interactive",
		"interactive|exec|all — which session modes to include (default hides non-interactive `codex exec`/headless runs)")
	pretty := fs.Bool("pretty", false, "human-readable table (default: compact JSON)")
	_ = fs.Parse(args)

	st := openStore()
	defer st.Close()

	// Reconcile the sessions registry with disk so new sessions show up without a
	// reindex. Cheap: unchanged files cost only a stat (open-only-changed).
	if err := index.RefreshSessions(context.Background(), st); err != nil {
		fmt.Fprintln(os.Stderr, "recall: warning: session refresh failed: "+err.Error())
	}

	f := store.Filter{Source: *source, Project: *project, Since: parseSince(*since)}
	all, err := st.ListSessions(f)
	if err != nil {
		fatal(err)
	}
	// Keep only the requested modes (mode is now a stored column, no file reads).
	sessions := all[:0]
	for _, s := range all {
		switch *mode {
		case "all":
		case "exec":
			if s.Mode != "exec" {
				continue
			}
		default: // "interactive"
			if s.Mode == "exec" {
				continue
			}
		}
		sessions = append(sessions, s)
	}
	if *limit > 0 && len(sessions) > *limit {
		sessions = sessions[:*limit]
	}

	if !*pretty {
		_ = json.NewEncoder(os.Stdout).Encode(sessions)
		return
	}
	if len(sessions) == 0 {
		fmt.Fprintln(os.Stderr, "no sessions (run `recall index` first)")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "DATETIME\tSOURCE\tSESSION\tPROJECT\tTITLE\tFLAGS")
	for _, s := range sessions {
		when := "—"
		if s.LastTs > 0 {
			when = time.Unix(s.LastTs, 0).Format("2006-01-02 15:04")
		}
		flags := ""
		if s.Pinned {
			flags += "📌"
		}
		if s.HasSummary {
			flags += "✶"
		}
		if s.Mode == "exec" {
			flags += "⚙"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			when, s.Source, s.SessionID,
			truncate(filepath.Base(s.Project), 22), truncate(s.Title, 50), flags)
	}
	_ = w.Flush()
}

// --- recall pin / unpin -----------------------------------------------------

func runPin(args []string, pin bool) {
	name := "pin"
	if !pin {
		name = "unpin"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	source := fs.String("source", "", "claude|codex (auto-detected if omitted)")
	flagArgs, pos := splitArgs(args)
	_ = fs.Parse(flagArgs)
	if len(pos) < 1 {
		fatal(fmt.Errorf("usage: recall %s <session-id> [--source claude|codex]", name))
	}
	id := pos[0]

	st := openStore()
	defer st.Close()

	src := *source
	if src == "" {
		s, _, _, ok, err := st.SessionByID(id)
		if err != nil {
			fatal(err)
		}
		if !ok {
			fatal(fmt.Errorf("session %q not found in index", id))
		}
		src = s
	}
	var err error
	if pin {
		err = st.Pin(src, id, time.Now().Unix())
	} else {
		err = st.Unpin(src, id)
	}
	if err != nil {
		fatal(err)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"pinned": pin, "source": src, "session_id": id,
	})
}

// --- recall resume ----------------------------------------------------------

func runResume(args []string) {
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	flagArgs, pos := splitArgs(args)
	_ = fs.Parse(flagArgs)
	if len(pos) < 1 {
		fatal(fmt.Errorf("usage: recall resume <session-id>"))
	}
	id := pos[0]

	st := openStore()
	defer st.Close()
	src, project, path, ok, err := st.SessionByID(id)
	if err != nil {
		fatal(err)
	}
	if !ok {
		fatal(fmt.Errorf("session %q not found in index", id))
	}
	cwd := project
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"source":     src,
		"session_id": id,
		"cwd":        cwd,
		"command":    resumeCommand(src, id),
		"path":       path,
	})
}

func resumeCommand(source, id string) string {
	switch source {
	case "codex":
		return "codex --dangerously-bypass-approvals-and-sandbox resume " + id
	default: // claude
		return "claude --dangerously-skip-permissions --resume " + id
	}
}

// --- recall summary ---------------------------------------------------------

const summaryPrompt = `You are summarizing a Claude Code / Codex coding-session transcript provided on stdin.
Goal: let me (a) recognize whether this is the session I want and (b) recall what was found/decided and what's left to do — WITHOUT re-reading it. Favor concrete outcomes, decisions, and findings over a table of contents.

Output PLAIN TEXT in exactly this shape (no markdown, no preamble, keep the labels):

Gist: <one sentence — the bottom line of the session>
Key points:
- <a concrete finding, decision, answer, or change — a full clause with specifics (the root cause, the number, the choice made, the file/service); NOT a topic heading>
- <3 to 6 bullets; fold in artifacts produced — docs, PRs, tasks, key files/commands>
Outcome: <where it ended — solved / shipped / blocked / self-resolved — and the bottom-line state>
Next: <unresolved items, pending decisions, or follow-ups, with any deadline; write a single — if nothing is left to do>

Rules: prefer concrete nouns (file paths, IDs, services, numbers) over vague phrasing; each Key point must say what was learned or done, not merely the subject. Be terse. Do NOT run any commands or tools — just read and summarize.`

func runSummary(args []string) {
	fs := flag.NewFlagSet("summary", flag.ExitOnError)
	model := fs.String("model", "", "codex model to use (default: codex's configured model)")
	refresh := fs.Bool("refresh", false, "ignore cache and regenerate")
	cachedOnly := fs.Bool("cached-only", false,
		"return the cached summary if one exists (ignoring staleness); never generate. Empty summary if none cached.")
	pretty := fs.Bool("pretty", false, "print just the summary text (default: JSON)")
	flagArgs, pos := splitArgs(args)
	_ = fs.Parse(flagArgs)
	if len(pos) < 1 {
		fatal(fmt.Errorf("usage: recall summary <session-id> [--refresh] [--cached-only] [--model NAME]"))
	}
	id := pos[0]

	st := openStore()
	defer st.Close()
	src, _, path, ok, err := st.SessionByID(id)
	if err != nil {
		fatal(err)
	}
	if !ok {
		fatal(fmt.Errorf("session %q not found in index", id))
	}
	mtime := fileMtimeNs(path)

	// Cache-only: used by the app on session open so glancing never auto-spends a
	// codex call. Ignore staleness (mtime 0) — an actively-growing session would
	// otherwise look stale on every open and trigger needless regeneration.
	if *cachedOnly {
		cached, hit, err := st.GetSummary(src, id, 0)
		if err != nil {
			fatal(err)
		}
		if hit {
			emitSummary(cached, src, id, true, *pretty)
		} else {
			emitSummary("", src, id, false, *pretty) // signal "no cache" without generating
		}
		return
	}

	if !*refresh {
		if cached, hit, err := st.GetSummary(src, id, mtime); err == nil && hit {
			emitSummary(cached, src, id, true, *pretty)
			return
		}
	}

	transcript := buildTranscriptText(path, src)
	if strings.TrimSpace(transcript) == "" {
		fatal(fmt.Errorf("no readable transcript at %s", path))
	}
	fmt.Fprintf(os.Stderr, "summarizing %s via codex…\n", id)
	summary, err := codexSummarize(transcript, *model)
	if err != nil {
		fatal(err)
	}
	if err := st.SetSummary(src, id, mtime, *model, summary, time.Now().Unix()); err != nil {
		fmt.Fprintln(os.Stderr, "recall: warning: failed to cache summary: "+err.Error())
	}
	emitSummary(summary, src, id, false, *pretty)
}

func emitSummary(summary, source, id string, cached, pretty bool) {
	if pretty {
		fmt.Println(summary)
		return
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"source": source, "session_id": id, "cached": cached, "summary": summary,
	})
}

// buildTranscriptText renders a session's messages into a plain-text conversation
// for the summarizer, capped (keeping head + tail) so long sessions stay within
// the model's context — the start and end matter most for a glance summary.
func buildTranscriptText(path, source string) string {
	recs, err := adapter.Parse(adapter.SessionFile{Path: path, Source: source})
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, r := range recs {
		b.WriteString(strings.ToUpper(r.Role))
		b.WriteString(":\n")
		b.WriteString(r.Text)
		b.WriteString("\n\n")
	}
	return capMiddle(b.String(), 48000)
}

func capMiddle(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	head := max * 2 / 3
	tail := max - head
	return string(r[:head]) + "\n\n…[transcript truncated]…\n\n" + string(r[len(r)-tail:])
}

func codexSummarize(transcript, model string) (string, error) {
	bin := resolveBin("codex",
		"/opt/homebrew/bin/codex", os.ExpandEnv("$HOME/.local/bin/codex"), "/usr/local/bin/codex")
	if bin == "" {
		return "", fmt.Errorf("codex CLI not found (needed for summaries)")
	}
	tmp, err := os.CreateTemp("", "recall-summary-*.txt")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	cmdArgs := []string{"exec", "--skip-git-repo-check", "-s", "read-only", "-o", tmpPath}
	if model != "" {
		cmdArgs = append(cmdArgs, "-m", model)
	}
	cmdArgs = append(cmdArgs, summaryPrompt)

	cmd := exec.Command(bin, cmdArgs...)
	cmd.Stdin = strings.NewReader(transcript)
	cmd.Stdout = io.Discard
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("codex exec failed: %v: %s", err, lastLines(stderr.String(), 5))
	}
	out, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", err
	}
	summary := strings.TrimSpace(string(out))
	if summary == "" {
		return "", fmt.Errorf("codex produced an empty summary: %s", lastLines(stderr.String(), 5))
	}
	return summary, nil
}

// --- small helpers ----------------------------------------------------------

func resolveBin(name string, candidates ...string) string {
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return c
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

func fileMtimeNs(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.ModTime().UnixNano()
}

func lastLines(s string, n int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
