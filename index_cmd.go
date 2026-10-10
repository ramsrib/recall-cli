// `recall index`: the indexing pass, its progress output, and its run history.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"
	"unsafe"

	"github.com/ramsrib/recall-cli/internal/embed"
	"github.com/ramsrib/recall-cli/internal/index"
	"github.com/ramsrib/recall-cli/internal/store"
)

// exitLocked is `recall index`'s exit status when another index run holds the
// lock (EX_TEMPFAIL: nothing is wrong, try again later).
const exitLocked = 75

// runFiles caps how many files a run record keeps: the slowest ones.
const runFiles = 10

func runIndex(args []string) {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	model := fs.String("model", "", "embedding model (default: $RECALL_EMBED_MODEL or qwen3-embedding:0.6b)")
	history := fs.Bool("history", false, "list recent index runs instead of indexing")
	limit := fs.Int("limit", 20, "with --history: max runs")
	pretty := fs.Bool("pretty", false, "with --history: human-readable table (default output is compact JSON)")
	_ = fs.Parse(args)
	if *history {
		runIndexHistory(*limit, *pretty)
		return
	}

	em := embed.New(*model)
	tty := isTerminal(os.Stderr)
	run := store.IndexRun{StartedAt: time.Now().Unix(), Model: em.Model, Host: em.Host(), Interactive: tty}
	start := time.Now()

	// Every outcome below ends in finishRun, so stdout always carries the run
	// record, even when the run never got going.
	unlock, holder, err := lockIndex()
	if err != nil {
		failRun(nil, &run, start, err)
	}
	if unlock == nil {
		// Recording a refused run is best-effort: the run holding the lock may
		// also hold SQLite's write lock for longer than the busy timeout.
		st, _ := tryOpenStore()
		run.Status = "locked"
		finishRun(st, &run, start)
		if st != nil {
			st.Close()
		}
		who := "another `recall index`"
		if holder != "" {
			who += " (pid " + holder + ")"
		}
		fmt.Fprintf(os.Stderr, "recall: %s is already running; the index will be current when it finishes\n", who)
		os.Exit(exitLocked)
	}
	defer unlock()
	st, err := tryOpenStore()
	if err != nil {
		unlock()
		failRun(nil, &run, start, err)
	}
	defer st.Close()

	// Ctrl-C stops between embedding batches. Each file commits on its own,
	// so everything finished so far is kept. A second Ctrl-C kills outright.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() { <-ctx.Done(); stop() }()

	p := newProgress(tty, em)
	stats, err := index.Run(ctx, st, em, run.StartedAt, p)
	p.done()

	run.New, run.Appended, run.Changed = stats.New, stats.Appended, stats.Changed
	run.Remodeled, run.Skipped, run.Deleted = stats.Remodeled, stats.Skipped, stats.Deleted
	run.Chunks, run.EmbedMs = stats.Chunks, stats.Embed.Milliseconds()
	run.Files = slowest(stats.Files, runFiles)
	switch {
	case ctx.Err() != nil:
		run.Status = "interrupted"
	case err != nil:
		run.Status, run.Error = "error", err.Error()
	default:
		run.Status = "ok"
	}
	finishRun(st, &run, start)
	summarize(st, run, em.Model)

	switch run.Status {
	case "interrupted":
		fmt.Fprintln(os.Stderr, "interrupted: finished files are kept; run `recall index` again to do the rest")
		unlock()
		st.Close()
		os.Exit(130)
	case "error":
		unlock()
		st.Close()
		fatal(err)
	}
}

// tryOpenStore opens the default index DB, returning the error instead of exiting.
func tryOpenStore() (*store.Store, error) {
	path, err := store.DefaultPath()
	if err != nil {
		return nil, err
	}
	return store.Open(path)
}

// failRun ends a run that could not start: it reports the error as the run
// record and exits 1.
func failRun(st *store.Store, run *store.IndexRun, start time.Time, err error) {
	run.Status, run.Error = "error", err.Error()
	finishRun(st, run, start)
	fatal(err)
}

// finishRun stamps the run's duration, records it if st is non-nil
// (best-effort: a failure to log must not change the run's outcome), and
// prints it to stdout as compact JSON for scripts and agents. Progress and
// the summary go to stderr.
func finishRun(st *store.Store, run *store.IndexRun, start time.Time) {
	run.DurationMs = time.Since(start).Milliseconds()
	if st != nil {
		if err := st.LogIndexRun(*run); err != nil {
			fmt.Fprintln(os.Stderr, "recall: warning: failed to log index run: "+err.Error())
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(run)
}

// summarize prints the end-of-run summary to stderr.
func summarize(st *store.Store, run store.IndexRun, model string) {
	took := fmtDur(time.Duration(run.DurationMs) * time.Millisecond)
	if run.EmbedMs > 0 {
		took += " (" + fmtDur(time.Duration(run.EmbedMs)*time.Millisecond) + " embedding)"
	}
	fmt.Fprintf(os.Stderr, "\n%s in %s: %s new, %s appended, %s changed, %s re-embedded (model change), %s unchanged, %s deleted · %s chunks embedded\n",
		map[string]string{"ok": "done", "interrupted": "stopped", "error": "failed"}[run.Status], took,
		fmtInt(run.New), fmtInt(run.Appended), fmtInt(run.Changed), fmtInt(run.Remodeled),
		fmtInt(run.Skipped), fmtInt(run.Deleted), fmtInt(run.Chunks))
	files, chunks := mustCounts(st)
	fmt.Fprintf(os.Stderr, "index holds %s sessions, %s chunks\n", fmtInt(files), fmtInt(chunks))
	if mc, err := st.ModelCounts(); err == nil && len(mc) > 1 {
		fmt.Fprintf(os.Stderr, "embedding models in index:\n")
		for m, n := range mc {
			fmt.Fprintf(os.Stderr, "  %-24s %d files\n", m, n)
		}
		fmt.Fprintf(os.Stderr, "re-run `recall index` to bring all files onto %s.\n", model)
	}
}

// slowest returns up to n files, slowest first, as run-record entries.
func slowest(files []index.FileStat, n int) []store.RunFile {
	fs := append([]index.FileStat(nil), files...)
	sort.SliceStable(fs, func(i, j int) bool { return fs[i].Took > fs[j].Took })
	var out []store.RunFile
	for _, f := range fs[:min(n, len(fs))] {
		out = append(out, store.RunFile{
			Path: f.Path, How: f.How, Chunks: f.Chunks,
			Ms: f.Took.Milliseconds(), EmbedMs: f.Embed.Milliseconds(),
		})
	}
	return out
}

func runIndexHistory(limit int, pretty bool) {
	st := openStore()
	defer st.Close()
	runs, err := st.RecentIndexRuns(limit)
	if err != nil {
		fatal(err)
	}
	if !pretty {
		_ = json.NewEncoder(os.Stdout).Encode(runs)
		return
	}
	if len(runs) == 0 {
		fmt.Fprintln(os.Stderr, "no index runs logged yet")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "DATETIME\tSTATUS\tTOOK\tEMBED\tNEW\tAPPENDED\tCHANGED\tCHUNKS\tHOST\tSLOWEST")
	for _, r := range runs {
		slow := ""
		if len(r.Files) > 0 {
			f := r.Files[0]
			slow = fmt.Sprintf("%s (%s, %s chunks)", truncate(filepath.Base(f.Path), 24),
				fmtDur(time.Duration(f.Ms)*time.Millisecond), fmtInt(f.Chunks))
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s\n",
			time.Unix(r.StartedAt, 0).Format("2006-01-02 15:04"), r.Status,
			fmtDur(time.Duration(r.DurationMs)*time.Millisecond), fmtDur(time.Duration(r.EmbedMs)*time.Millisecond),
			r.New, r.Appended, r.Changed, fmtInt(r.Chunks), shortHost(r.Host), slow)
	}
	_ = w.Flush()
}

// lockIndex makes `recall index` single-instance. Two passes over the same DB
// would embed the same files twice and contend on SQLite's write lock: a
// manual run alongside the scheduled one, say, or a GUI client's refresh. The
// lock is a flock on a file next to the DB, so the kernel drops it when the
// process exits, however it exits. The holder writes its pid into the file for
// the message a refused run prints.
//
// If another run holds the lock, unlock is nil and holder is that run's pid,
// or "" if it isn't known yet (or names a process that is gone).
func lockIndex() (unlock func(), holder string, err error) {
	dbPath, err := store.DefaultPath()
	if err != nil {
		return nil, "", err
	}
	f, err := os.OpenFile(filepath.Join(filepath.Dir(dbPath), "index.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, "", err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		defer f.Close()
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, "", err
		}
		b := make([]byte, 16)
		n, _ := f.ReadAt(b, 0)
		pid, perr := strconv.Atoi(strings.TrimSpace(string(b[:n])))
		if perr != nil || pid <= 0 || syscall.Kill(pid, 0) == syscall.ESRCH {
			return nil, "", nil
		}
		return nil, strconv.Itoa(pid), nil
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = f.Truncate(0) // so a later refused run never names this pid
			f.Close()
		})
	}, "", nil
}

// --- progress ---------------------------------------------------------------

// heartbeat is how often line mode reports a file that is still embedding.
const heartbeat = 15 * time.Second

// progress prints index events to stderr. On a terminal, the file in flight
// is one line rewritten in place and every finished file gets a permanent
// line. Anywhere else (an agent, a log, a GUI client reading lines), output
// is plain lines: one per finished file, plus a heartbeat every few seconds
// for a file that takes long, so a slow pass never looks hung and a fast one
// stays short. A ticker keeps both going while a single embedding request is
// slow, since no event arrives until it returns.
type progress struct {
	tty  bool
	em   *embed.Client
	stop chan struct{}

	mu     sync.Mutex // guards the rest; Run's goroutine and the ticker both print
	todo   int
	i      int
	path   string
	msg    string    // what the file in flight is doing
	active bool      // a file is in flight
	since  time.Time // when it started
	last   time.Time // when line mode last printed anything
	live   bool      // a rewritable line is on screen
}

func newProgress(tty bool, em *embed.Client) *progress {
	p := &progress{tty: tty, em: em, stop: make(chan struct{}), last: time.Now()}
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-t.C:
				p.mu.Lock()
				if p.active {
					p.status()
				}
				p.mu.Unlock()
			}
		}
	}()
	return p
}

func (p *progress) Planned(todo, total int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.todo = todo
	if todo == 0 {
		p.println(fmt.Sprintf("all %s sessions are up to date", fmtInt(total)))
		return
	}
	p.println(fmt.Sprintf("indexing %s of %s sessions with %s on %s",
		fmtInt(todo), fmtInt(total), p.em.Model, shortHost(p.em.Host())))
}

func (p *progress) Started(i int, path, _ string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.i, p.path, p.since, p.active, p.msg = i, path, time.Now(), true, "reading"
	p.status()
}

func (p *progress) Embedded(done, total int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msg = fmt.Sprintf("embedding %s/%s chunks", fmtInt(done), fmtInt(total))
	if total > 0 {
		p.msg += fmt.Sprintf(" (%d%%)", done*100/total)
	}
	p.status()
}

func (p *progress) Finished(i int, f index.FileStat) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active = false
	what := fmtInt(f.Chunks) + " chunks"
	if f.How == "appended" {
		what = "+" + what
	}
	p.println(fmt.Sprintf("  %s  %-9s %14s %7s  %s", p.counter(i), f.How, what, fmtDur(f.Took), trimHome(f.Path)))
}

// Note comes between files: before the first, after the last, or in place of
// Finished for a file that was skipped. Either way no file is in flight.
func (p *progress) Note(msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active = false
	p.println("  note: " + msg)
}

// done stops the ticker and removes the rewritable line.
func (p *progress) done() {
	close(p.stop)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active = false
	p.clear()
}

func (p *progress) counter(i int) string {
	w := len(strconv.Itoa(p.todo))
	return fmt.Sprintf("[%*d/%d]", w, i, p.todo)
}

// status reports the file in flight: rewritten in place on a terminal, a
// heartbeat line elsewhere. Callers hold p.mu.
func (p *progress) status() {
	line := fmt.Sprintf("  %s  %s  %s  %s", p.counter(p.i), p.msg, fmtDur(time.Since(p.since)), trimHome(p.path))
	if p.tty {
		fmt.Fprint(os.Stderr, "\r\033[K"+fitWidth(line, termWidth()))
		p.live = true
		return
	}
	if time.Since(p.last) >= heartbeat && time.Since(p.since) >= heartbeat {
		p.println(line)
	}
}

// println prints a permanent line. Callers hold p.mu.
func (p *progress) println(line string) {
	p.clear()
	fmt.Fprintln(os.Stderr, line)
	p.last = time.Now()
}

// clear removes the rewritable line, if one is showing. Callers hold p.mu.
func (p *progress) clear() {
	if p.live {
		fmt.Fprint(os.Stderr, "\r\033[K")
		p.live = false
	}
}

// --- formatting -------------------------------------------------------------

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// termWidth is stderr's terminal width in columns, 80 if unknown.
func termWidth() int {
	var ws struct{ Row, Col, X, Y uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, os.Stderr.Fd(),
		uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if errno != 0 || ws.Col == 0 {
		return 80
	}
	return int(ws.Col)
}

// fitWidth shortens s to fit in width columns, dropping the middle of it: a
// line that wraps can't be rewritten in place.
func fitWidth(s string, width int) string {
	r := []rune(s)
	if len(r) < width {
		return s
	}
	if width < 8 { // too narrow to elide sensibly
		return string(r[:max(width-1, 0)])
	}
	keep := width - 2
	head := keep * 2 / 3
	return string(r[:head]) + "…" + string(r[len(r)-(keep-head):])
}

func fmtDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// fmtInt formats n with thousands separators.
func fmtInt(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func shortHost(h string) string {
	return strings.TrimPrefix(strings.TrimPrefix(h, "http://"), "https://")
}

func trimHome(p string) string {
	if h, err := os.UserHomeDir(); err == nil {
		if rel := strings.TrimPrefix(p, h); rel != p {
			return "~" + rel
		}
	}
	return p
}
