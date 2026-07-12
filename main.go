// recall — local-first semantic + full-text search over Claude Code and Codex sessions.
//
//	recall index                                   reindex changed sessions (both tools)
//	recall "how did I fix pyexpat" [flags]         search
//	recall log                                     recent searches
//
// Session-manager commands (list, pin, unpin, summary, resume) live in sessions_cmd.go.
//
// Search flags: --mode hybrid|semantic|lexical  --source claude|codex
//               --project SUBSTR  --since YYYY-MM-DD  --limit N  --model NAME
//               --all  --include-exec  --pretty
//
// Indexing and search are local: embeddings come from Ollama on OLLAMA_HOST
// (localhost by default). The two ways content leaves the machine are `recall
// summary`, which pipes transcript text to `codex exec`, and a non-local
// OLLAMA_HOST. See the README's "Privacy & network behavior".
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ramsrib/recall-cli/internal/embed"
	"github.com/ramsrib/recall-cli/internal/index"
	"github.com/ramsrib/recall-cli/internal/search"
	"github.com/ramsrib/recall-cli/internal/store"
)

// Set at release time via -ldflags "-X main.version=...". Stays "dev" for
// builds made straight from the tree.
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "-v", "--version", "version":
		fmt.Println("recall", version)
	case "index":
		runIndex(os.Args[2:])
	case "log":
		runLog(os.Args[2:])
	case "list":
		runList(os.Args[2:])
	case "pin":
		runPin(os.Args[2:], true)
	case "unpin":
		runPin(os.Args[2:], false)
	case "summary":
		runSummary(os.Args[2:])
	case "resume":
		runResume(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		runSearch(os.Args[1:])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `recall — search Claude Code + Codex session history

Usage:
  recall index [--model NAME]
  recall "QUERY" [--mode hybrid|semantic|lexical] [--source claude|codex]
                 [--project SUBSTR] [--since YYYY-MM-DD] [--limit N]
                 [--model NAME] [--include-exec] [--pretty]
                 (non-interactive exec/headless sessions are hidden unless --include-exec)
  recall "A B C" --all          sessions containing ALL keywords (anywhere)
  recall log [--limit N] [--pretty]            recent searches (every query is logged)

Session manager (also powers the recall-app desktop client):
  recall list [--source S] [--project P] [--since D] [--limit N]
              [--mode interactive|exec|all] [--pretty]
                                one row per session (+ pin/summary state);
                                hides non-interactive exec runs unless --mode
  recall pin <session-id>       pin a session     (recall unpin <id> to remove)
  recall summary <session-id> [--refresh] [--cached-only] [--model NAME] [--pretty]
                                concise overview via codex, cached per session.
                                SENDS TRANSCRIPT TEXT to codex exec (network) —
                                the only command that leaves the machine. Needs an
                                installed + authenticated codex CLI.
  recall resume <session-id>    PRINT (never run) the resume command + cwd. The
                                command it prints disables the agent's safety
                                rails (--dangerously-skip-permissions /
                                --dangerously-bypass-approvals-and-sandbox).

Models:
  index --model NAME re-embeds only files not already on NAME (incremental).
  Search defaults to the index's current model; --model queries another model's vectors.

Output:
  Default is compact, single-line JSON (incl. transcript path) — agent-first,
  no flag needed when calling from a script or agent.
  Pass --pretty for a human-readable table (DATETIME SOURCE SESSION PROJECT TITLE).
`)
}

func runIndex(args []string) {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	model := fs.String("model", "", "embedding model (default: $RECALL_EMBED_MODEL or qwen3-embedding:0.6b)")
	_ = fs.Parse(args)

	st := openStore()
	defer st.Close()
	em := embed.New(*model)

	fmt.Fprintf(os.Stderr, "indexing (model=%s)…\n", em.Model)
	stats, err := index.Run(st, em, time.Now().Unix(), func(s string) {
		fmt.Fprintln(os.Stderr, s)
	})
	if err != nil {
		fatal(err)
	}
	files, chunks := mustCounts(st)
	fmt.Fprintf(os.Stderr, "\ndone: +%d new, %d changed, %d re-embedded (model change), %d skipped, %d deleted (%d chunks this run)\n",
		stats.New, stats.Changed, stats.Remodeled, stats.Skipped, stats.Deleted, stats.Chunks)
	fmt.Fprintf(os.Stderr, "index now holds %d sessions/files, %d chunks\n", files, chunks)
	if mc, err := st.ModelCounts(); err == nil && len(mc) > 1 {
		fmt.Fprintf(os.Stderr, "embedding models in index:\n")
		for m, n := range mc {
			fmt.Fprintf(os.Stderr, "  %-24s %d files\n", m, n)
		}
		fmt.Fprintf(os.Stderr, "re-run `recall index` to bring all files onto %s.\n", em.Model)
	}
}

func runSearch(args []string) {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	mode := fs.String("mode", "hybrid", "hybrid|semantic|lexical")
	source := fs.String("source", "", "claude|codex")
	project := fs.String("project", "", "substring match on project path")
	since := fs.String("since", "", "only sessions on/after this date (YYYY-MM-DD)")
	limit := fs.Int("limit", 20, "max results")
	all := fs.Bool("all", false, "require ALL keywords somewhere in the session (lexical, session-level AND)")
	pretty := fs.Bool("pretty", false, "human-readable table (default output is compact JSON)")
	model := fs.String("model", "", "embedding model for semantic ranking (default: the index's current model)")
	includeExec := fs.Bool("include-exec", false, "include non-interactive `codex exec`/headless sessions (hidden by default)")

	// Allow flags before OR after the query: split positional from flag tokens.
	flagArgs, pos := splitArgs(args)
	_ = fs.Parse(flagArgs)
	query := strings.TrimSpace(strings.Join(pos, " "))
	if query == "" {
		usage()
		os.Exit(2)
	}

	st := openStore()
	defer st.Close()

	f := store.Filter{Source: *source, Project: *project, Since: parseSince(*since), ExcludeExec: !*includeExec}
	var results []search.Result
	var err error
	usedMode, usedModel := *mode, ""
	start := time.Now()
	if *all {
		// session-level AND — pure lexical, no embeddings needed
		usedMode = "all"
		results, err = search.RunAll(st, query, f, *limit)
	} else {
		// Query model: explicit --model, else the index's current model. We
		// embed the query with it and rank only chunks stored under it.
		qmodel := *model
		if qmodel == "" {
			qmodel, _ = st.GetMeta("embed_model")
		}
		if qmodel == "" {
			fatal(fmt.Errorf("index is empty — run `recall index` first"))
		}
		em := embed.New(qmodel)
		if *mode != string(search.ModeLexical) {
			usedModel = em.Model // lexical doesn't embed, so no model "used"
		}
		results, err = search.Run(st, em, query, search.Mode(*mode), f, *limit, nil)
		if err == nil && *mode != string(search.ModeLexical) {
			warnExcluded(st, em.Model)
		}
	}
	if err != nil {
		fatal(err)
	}
	logSearch(st, query, usedMode, usedModel, f, *limit, results, time.Since(start))

	// Agent-first: default output is compact, single-line JSON (incl. the
	// transcript `path`), so a script/agent gets structured records with no
	// flag. --pretty renders a human table.
	if !*pretty {
		enc := json.NewEncoder(os.Stdout) // compact: no indentation, one line
		_ = enc.Encode(results)
		return
	}
	if len(results) == 0 {
		fmt.Fprintln(os.Stderr, "no matches")
		return
	}
	// SESSION is first after date+source: it's the value you copy to resume a session.
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "DATETIME\tSOURCE\tSESSION\tPROJECT\tTITLE")
	for _, r := range results {
		when := "—"
		if r.Ts > 0 {
			when = time.Unix(r.Ts, 0).Format("2006-01-02 15:04") // local, minute resolution
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			when, r.Source, r.SessionID,
			truncate(filepath.Base(r.Project), 24), truncate(r.Title, 60))
	}
	_ = w.Flush()
}

// logSearch records the search in the query log (best-effort, non-optional).
// A logging failure only warns — it never affects the search the user just ran.
func logSearch(st *store.Store, query, mode, model string, f store.Filter, limit int, results []search.Result, dur time.Duration) {
	top := make([]string, 0, 10)
	for _, r := range results {
		if len(top) >= 10 {
			break
		}
		top = append(top, r.Source+"/"+r.SessionID)
	}
	err := st.LogQuery(store.QueryLog{
		Ts: time.Now().Unix(), Query: query, Mode: mode,
		Source: f.Source, Project: f.Project, Since: f.Since, Limit: limit,
		Model: model, NResults: len(results), TopSessions: top,
		DurationMs: dur.Milliseconds(),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "recall: warning: failed to log query: "+err.Error())
	}
}

// runLog prints recently logged searches (newest first).
func runLog(args []string) {
	fs := flag.NewFlagSet("log", flag.ExitOnError)
	limit := fs.Int("limit", 20, "max entries")
	pretty := fs.Bool("pretty", false, "human-readable table (default output is compact JSON)")
	_ = fs.Parse(args)

	st := openStore()
	defer st.Close()
	qs, err := st.RecentQueries(*limit)
	if err != nil {
		fatal(err)
	}

	if !*pretty {
		_ = json.NewEncoder(os.Stdout).Encode(qs)
		return
	}
	if len(qs) == 0 {
		fmt.Fprintln(os.Stderr, "no queries logged yet")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "DATETIME\tMODE\tMODEL\tN\tms\tQUERY")
	for _, q := range qs {
		when := time.Unix(q.Ts, 0).Format("2006-01-02 15:04")
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%s\n",
			when, q.Mode, truncate(q.Model, 20), q.NResults, q.DurationMs, truncate(q.Query, 50))
	}
	_ = w.Flush()
}

// warnExcluded notes (on stderr, so JSON on stdout stays clean) when some
// indexed sessions can't participate in semantic ranking because they were
// embedded with a different model than the query is using.
func warnExcluded(st *store.Store, model string) {
	n, err := st.FilesNotOnModel(model)
	if err != nil || n == 0 {
		return
	}
	fmt.Fprintf(os.Stderr,
		"note: %d session(s) embedded under another model were excluded from semantic ranking; run `recall index --model %s` to re-embed them.\n",
		n, model)
}

// --- helpers ----------------------------------------------------------------

// splitArgs separates flag tokens (and their values) from positional query words,
// so flags may appear before or after the query.
func splitArgs(args []string) (flags, pos []string) {
	boolFlags := map[string]bool{
		"--pretty": true, "-pretty": true, "--all": true, "-all": true,
		"--refresh": true, "-refresh": true,
		"--include-exec": true, "-include-exec": true,
		"--cached-only": true, "-cached-only": true,
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			if !boolFlags[a] && !strings.Contains(a, "=") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		pos = append(pos, a)
	}
	return
}

func openStore() *store.Store {
	path, err := store.DefaultPath()
	if err != nil {
		fatal(err)
	}
	st, err := store.Open(path)
	if err != nil {
		fatal(err)
	}
	return st
}

func mustCounts(st *store.Store) (int, int) {
	f, c, err := st.Counts()
	if err != nil {
		fatal(err)
	}
	return f, c
}

func parseSince(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		fatal(fmt.Errorf("bad --since %q (want YYYY-MM-DD)", s))
	}
	return t.Unix()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "recall: "+err.Error())
	os.Exit(1)
}
