package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Status constants
const (
	StatusIdle    = 0
	StatusWaiting = 1
	StatusWorking = 2
)

// Actions performed after the TUI exits (see main).
const (
	actionNone   = 0
	actionSwitch = 1 // switch-client to a live pane
	actionOpen   = 2 // spawn a new session in a project folder, then switch
)

// maxProjects caps the recent-projects list in the *unfiltered* view so the
// popup stays readable. Older projects remain reachable via search.
const maxProjects = 25

// projectRefreshInterval throttles the (relatively expensive) walk of
// ~/.claude/projects. Live sessions still refresh every tick; only the recent
// list, which changes slowly, is cached between walks.
const projectRefreshInterval = 3 * time.Second

type ClaudeSession struct {
	PaneID      string
	SessionName string
	Title       string
	Path        string // ~-shortened cwd, for display
	Dir         string // raw cwd, for spawning a new session in the same folder
	Status      int
}

// Project is a folder previously used with Claude that is not currently open.
// Path is the real absolute cwd, recovered from a transcript (see readCwd);
// Canon is that path with symlinks resolved, used to dedup against live panes.
type Project struct {
	Path    string
	Canon   string
	Display string // ~-shortened path
	Name    string // basename, used as the row label
	ModTime time.Time
}

// row is one selectable line in the combined list (a live session or a project).
type rowKind int

const (
	rowSession rowKind = iota
	rowProject
)

type row struct {
	kind rowKind
	sess ClaudeSession
	proj Project
}

func rowKey(r row) string {
	if r.kind == rowSession {
		return "s:" + r.sess.PaneID
	}
	return "p:" + r.proj.Path
}

// rowFolder returns the folder a row points at (a live session's cwd or a
// project's path) and its display form, for spawning a new session there.
func rowFolder(r row) (dir, display string, ok bool) {
	if r.kind == rowSession {
		if r.sess.Dir == "" {
			return "", "", false
		}
		return r.sess.Dir, r.sess.Path, true
	}
	return r.proj.Path, r.proj.Display, true
}

// scanResult bundles a single scan of live sessions plus closed projects.
type scanResult struct {
	sessions []ClaudeSession
	projects []Project
}

// Messages
type scanResultMsg scanResult
type tickMsg time.Time

// Commands
func scan() tea.Cmd {
	return func() tea.Msg {
		return scanResultMsg(scanAll())
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// killSession kills a tmux session by name, then rescans.
func killSession(name string) tea.Cmd {
	return func() tea.Msg {
		exec.Command("tmux", "kill-session", "-t", name).Run()
		return scanResultMsg(scanAll())
	}
}

// scanAll gathers live sessions first, then recent projects (excluding any
// folder that already has a live session).
func scanAll() scanResult {
	sessions, livePaths := detectSessions()
	projects := detectProjects(livePaths)
	return scanResult{sessions: sessions, projects: projects}
}

// Detection pipeline

// procTree maps a parent PID to its child PIDs, plus each PID's comm name.
// It is used to detect a live `claude` process anywhere in a pane's subtree.
// This is more reliable than tmux's pane_current_command, which reports only
// the foreground process-group leader: a Claude launched under a login shell
// frequently shows up as "zsh" even while it is alive and rendering.
type procTree struct {
	children map[int][]int
	comm     map[int]string
}

// buildProcTree snapshots all processes once via `ps`.
// `-eo` is portable across macOS and Linux; comm is the executable basename
// (truncated to 15 chars on Linux, which still fits "claude").
func buildProcTree() *procTree {
	t := &procTree{children: map[int][]int{}, comm: map[int]string{}}
	out, err := exec.Command("ps", "-eo", "pid=,ppid=,comm=").Output()
	if err != nil {
		return t
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		// comm is the basename of the executable; preserve it verbatim.
		t.comm[pid] = fields[2]
		t.children[ppid] = append(t.children[ppid], pid)
	}
	return t
}

// hasClaude reports whether pid or any descendant is a `claude` process.
func (t *procTree) hasClaude(pid int) bool {
	return t.hasClaudeRec(pid, map[int]bool{})
}

func (t *procTree) hasClaudeRec(pid int, seen map[int]bool) bool {
	if seen[pid] {
		return false // guard against malformed (cyclic) snapshots
	}
	seen[pid] = true
	if t.comm[pid] == "claude" {
		return true
	}
	for _, c := range t.children[pid] {
		if t.hasClaudeRec(c, seen) {
			return true
		}
	}
	return false
}

// isClaudeTitle returns true if the title starts with ✳ or a Braille spinner (U+2800–U+28FF).
func isClaudeTitle(title string) bool {
	if title == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(title)
	return r == '✳' || (r >= 0x2800 && r <= 0x28FF)
}

// isBraillePrefix returns true if the first rune is a Braille spinner character.
func isBraillePrefix(title string) bool {
	if title == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(title)
	return r >= 0x2800 && r <= 0x28FF
}

// cleanTitle strips the ✳ or Braille prefix from the title for display.
func cleanTitle(title string) string {
	r, size := utf8.DecodeRuneInString(title)
	if r == '✳' || (r >= 0x2800 && r <= 0x28FF) {
		return strings.TrimSpace(title[size:])
	}
	return title
}

// canonPath resolves symlinks (and normalizes /tmp vs /private/tmp on macOS) so
// a live pane's path and a transcript's recorded cwd compare equal even when
// they differ only by canonicalization. Falls back to a lexical clean.
func canonPath(p string) string {
	if p == "" {
		return p
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// detectSessions returns the live Claude sessions and the set of canonical pane
// paths they occupy (used to hide already-open folders from the recent list).
func detectSessions() ([]ClaudeSession, map[string]bool) {
	livePaths := map[string]bool{}

	// Step 1: list all panes. pane_pid roots the per-pane process-tree lookup
	// used for the liveness check below.
	out, err := exec.Command("tmux", "list-panes", "-a", "-F",
		"#{session_name}:#{window_index}.#{pane_index}\t#{pane_current_path}\t#{pane_title}\t#{pane_pid}").Output()
	if err != nil {
		return nil, livePaths
	}

	// Snapshot the process tree once; used to confirm a live claude process
	// exists in each candidate pane's subtree (see hasClaude).
	tree := buildProcTree()

	type paneInfo struct {
		id      string
		sess    string
		path    string
		title   string
		working bool // title has Braille spinner prefix
	}

	var candidates []paneInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) < 4 {
			continue
		}
		title := parts[2]

		// Check A: title must start with ✳ or Braille spinner
		if !isClaudeTitle(title) {
			continue
		}
		// Check B: a live `claude` process must exist in the pane's subtree.
		// (pane_current_command is unreliable — it reports the foreground
		// process-group leader, often "zsh" even while Claude is running.)
		panePID, err := strconv.Atoi(parts[3])
		if err != nil || !tree.hasClaude(panePID) {
			continue
		}

		paneID := parts[0]
		sessName := strings.SplitN(paneID, ":", 2)[0]
		candidates = append(candidates, paneInfo{
			id:      paneID,
			sess:    sessName,
			path:    parts[1],
			title:   cleanTitle(title),
			working: isBraillePrefix(title),
		})
	}

	if len(candidates) == 0 {
		return nil, livePaths
	}

	// Step 2: determine status in parallel
	// Working sessions (Braille prefix) need no capture-pane call.
	// Idle/Waiting sessions (✳ prefix) capture content to distinguish.
	results := make([]ClaudeSession, len(candidates))
	valid := make([]bool, len(candidates))
	var wg sync.WaitGroup

	for i, c := range candidates {
		wg.Add(1)
		go func(idx int, p paneInfo) {
			defer wg.Done()

			status := StatusIdle
			if p.working {
				status = StatusWorking
			} else {
				// ✳ prefix — capture pane to distinguish Waiting vs Idle
				out, err := exec.Command("tmux", "capture-pane", "-t", p.id, "-p", "-S", "-50").Output()
				if err != nil {
					return
				}
				content := string(out)
				status = determineStatus(content)
			}

			results[idx] = ClaudeSession{
				PaneID:      p.id,
				SessionName: p.sess,
				Title:       p.title,
				Path:        shortenPath(p.path),
				Dir:         p.path,
				Status:      status,
			}
			valid[idx] = true
		}(i, c)
	}
	wg.Wait()

	// Only panes that survived status determination count as live. Building
	// livePaths from the valid set (rather than every candidate) ensures a pane
	// dropped here doesn't also get hidden from the recent-projects list.
	var sessions []ClaudeSession
	for i, v := range valid {
		if v {
			sessions = append(sessions, results[i])
			livePaths[canonPath(candidates[i].path)] = true
		}
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].PaneID < sessions[j].PaneID
	})

	return sessions, livePaths
}

// projPathCache maps a transcript file path to the cwd it records. cwd is stable
// per transcript, so this is populated once per file. Keying by file path (not
// by the lossy encoded dir name) means the newest transcript always governs the
// resolved cwd, and a fresh transcript is re-read rather than serving a stale
// value. Empty resolutions are never cached, so a transient miss is retried.
var (
	projPathMu    sync.Mutex
	projPathCache = map[string]string{}
)

// projectListCache memoizes the walk of ~/.claude/projects (the expensive part
// of a scan). Live-vs-recent filtering is cheap and still runs every tick.
var (
	projListMu   sync.Mutex
	projListData []Project
	projListAt   time.Time
	projListInit bool
)

// detectProjects returns recent-project folders not currently open. The heavy
// directory walk is cached (see loadAllProjects); the per-call work is just the
// livePaths filter, so this stays cheap on the 1-second tick.
func detectProjects(livePaths map[string]bool) []Project {
	all := loadAllProjects()
	var out []Project
	for _, p := range all {
		if livePaths[p.Canon] {
			continue // already open — shown in the live list
		}
		out = append(out, p)
	}
	return out
}

// loadAllProjects returns every existing folder previously used with Claude,
// most-recent first, refreshing the underlying walk at most once per interval.
func loadAllProjects() []Project {
	projListMu.Lock()
	if projListInit && time.Since(projListAt) < projectRefreshInterval {
		data := projListData
		projListMu.Unlock()
		return data
	}
	projListMu.Unlock()

	data := scanAllProjects()

	projListMu.Lock()
	projListData = data
	projListAt = time.Now()
	projListInit = true
	projListMu.Unlock()
	return data
}

// scanAllProjects walks ~/.claude/projects, recovering each folder's real cwd
// from its newest transcript and dropping folders that no longer exist.
func scanAllProjects() []Project {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	root := filepath.Join(home, ".claude", "projects")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	var projects []Project
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		newest, mtime := newestJSONL(dir)
		if newest == "" {
			continue
		}
		path := projectPath(newest)
		if path == "" {
			continue
		}
		if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
			continue // folder was moved or deleted
		}
		projects = append(projects, Project{
			Path:    path,
			Canon:   canonPath(path),
			Display: shortenPath(path),
			Name:    filepath.Base(path),
			ModTime: mtime,
		})
	}

	sort.Slice(projects, func(i, j int) bool {
		return projects[i].ModTime.After(projects[j].ModTime)
	})
	return projects
}

// newestJSONL returns the path and mtime of the most recently modified *.jsonl
// transcript directly inside dir. The mtime doubles as the project's recency.
func newestJSONL(dir string) (string, time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", time.Time{}
	}
	var best string
	var bestT time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(bestT) {
			bestT = info.ModTime()
			best = filepath.Join(dir, e.Name())
		}
	}
	return best, bestT
}

// projectPath resolves and caches a transcript's cwd. Misses are not cached so
// a not-yet-written cwd is retried on a later scan.
func projectPath(jsonlPath string) string {
	projPathMu.Lock()
	if v, ok := projPathCache[jsonlPath]; ok {
		projPathMu.Unlock()
		return v
	}
	projPathMu.Unlock()

	p := readCwd(jsonlPath)
	if p == "" {
		return ""
	}

	projPathMu.Lock()
	projPathCache[jsonlPath] = p
	projPathMu.Unlock()
	return p
}

// readCwd returns the first non-empty "cwd" field found in a transcript.
// bufio.Reader.ReadBytes handles arbitrarily long lines (transcripts can embed
// large content), unlike a fixed-buffer Scanner which would silently give up on
// an oversized line. Reading stops at the first match, or after a bounded number
// of lines since cwd always appears near the top.
func readCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	r := bufio.NewReader(f)
	for lines := 0; lines < 500; lines++ {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var o struct {
				Cwd string `json:"cwd"`
			}
			if json.Unmarshal(line, &o) == nil && o.Cwd != "" {
				return o.Cwd
			}
		}
		if err != nil {
			break
		}
	}
	return ""
}

func determineStatus(content string) int {
	// Only called for ✳-prefixed (non-working) sessions.
	// Distinguish Waiting (user input requested) vs Idle.
	// Only check content AFTER the last prompt to avoid stale matches.
	lines := strings.Split(content, "\n")
	lastPrompt := -1
	for i, line := range lines {
		if strings.Contains(line, "❯") {
			lastPrompt = i
		}
	}

	if lastPrompt >= 0 && lastPrompt < len(lines)-1 {
		afterPrompt := strings.Join(lines[lastPrompt+1:], "\n")
		if strings.Contains(afterPrompt, "Esc to cancel") {
			return StatusWaiting
		}
	}
	return StatusIdle
}

func shortenPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if strings.HasPrefix(path, home) {
		return "~" + path[len(home):]
	}
	return path
}

// openInFolder spawns a detached tmux session in dir, then switches the client
// to it. When resume is true it continues the folder's most recent conversation
// (`claude --continue`); otherwise it starts a fresh one — which is what you
// want when opening a second session for a folder that already has one live.
// The new-session error is checked so a name collision doesn't switch the
// client into an unrelated existing session.
func openInFolder(dir string, resume bool) {
	name := sessionNameFor(dir)
	cmd := "claude --dangerously-skip-permissions"
	if resume {
		cmd = "claude --continue --dangerously-skip-permissions"
	}
	if err := exec.Command("tmux", "new-session", "-d", "-s", name, "-c", dir, cmd).Run(); err != nil {
		return
	}
	exec.Command("tmux", "switch-client", "-t", name).Run()
}

// sessionNameFor derives a unique, tmux-safe session name from a folder path.
// tmux forbids '.' and ':' in session names; spaces are replaced for legibility.
// Uniqueness is by deterministic suffix rather than a timestamp so two folders
// opened in the same second can't collide.
func sessionNameFor(dir string) string {
	base := filepath.Base(dir)
	repl := strings.NewReplacer(".", "_", ":", "_", " ", "_")
	base = repl.Replace(base)
	if base == "" {
		base = "claude"
	}
	name := base
	for i := 2; sessionExists(name); i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

func sessionExists(name string) bool {
	return exec.Command("tmux", "has-session", "-t", "="+name).Run() == nil
}

// trimLastRune drops the final UTF-8 rune from s (backspace in the filter).
func trimLastRune(s string) string {
	if s == "" {
		return s
	}
	_, size := utf8.DecodeLastRuneInString(s)
	return s[:len(s)-size]
}

// Bubble Tea model

type model struct {
	sessions    []ClaudeSession
	projects    []Project
	cursor      int // index into the visible (filtered) row list
	width       int
	height      int
	quitting    bool
	action      int
	selectedID  string // pane id for actionSwitch
	openPath    string // folder for actionOpen
	filtering   bool   // search input is active
	filter      string // current search query
	confirmKill bool   // awaiting y/n confirmation for a kill
	killTarget  string // tmux session name to kill on confirm
	confirmOpen bool   // awaiting y/n confirmation to spawn a session
	openTarget  string // folder to open on confirm
	openDisplay string // ~-shortened folder, shown in the confirm prompt
	openResume  bool   // spawn with --continue (resume) vs a fresh conversation
}

// visibleRows builds the ordered, filtered list the cursor indexes into:
// live sessions first, then recent projects. Each search term (space-separated)
// must appear somewhere in a row for it to match. The maxProjects cap applies
// only when no filter is active, so search can reach the full history.
func (m model) visibleRows() []row {
	terms := strings.Fields(strings.ToLower(m.filter))
	match := func(hay string) bool {
		if len(terms) == 0 {
			return true
		}
		h := strings.ToLower(hay)
		for _, t := range terms {
			if !strings.Contains(h, t) {
				return false
			}
		}
		return true
	}

	var rows []row
	for _, s := range m.sessions {
		if match(s.SessionName + " " + s.Title + " " + s.Path) {
			rows = append(rows, row{kind: rowSession, sess: s})
		}
	}
	shown := 0
	for _, p := range m.projects {
		if !match(p.Name + " " + p.Display) {
			continue
		}
		rows = append(rows, row{kind: rowProject, proj: p})
		shown++
		if len(terms) == 0 && shown >= maxProjects {
			break
		}
	}
	return rows
}

// currentKey returns the stable key of the highlighted row (before a list
// update), so the selection can be re-anchored afterwards.
func (m model) currentKey() string {
	rows := m.visibleRows()
	if m.cursor < 0 || m.cursor >= len(rows) {
		return ""
	}
	return rowKey(rows[m.cursor])
}

// selectByKey moves the cursor to the row with the given key, or to the top if
// that row is no longer present (e.g. it fell outside the cap, or was closed).
// Anchoring by key — never leaving the cursor on a now-different row — is what
// keeps Enter from acting on the wrong session/folder after a refresh.
func (m *model) selectByKey(key string) {
	m.cursor = 0
	if key == "" {
		return
	}
	for i, r := range m.visibleRows() {
		if rowKey(r) == key {
			m.cursor = i
			return
		}
	}
}

// moveCursor advances the cursor by delta, wrapping around the visible rows.
func (m *model) moveCursor(rows []row, delta int) {
	n := len(rows)
	if n == 0 {
		m.cursor = 0
		return
	}
	m.cursor = ((m.cursor+delta)%n + n) % n
}

// activate performs the row's action: switch to a live session immediately, or
// (for a project) request confirmation before spawning a new session — so a
// stray Enter or number key can't launch Claude unattended.
func (m model) activate(rows []row, i int) (tea.Model, tea.Cmd) {
	if i < 0 || i >= len(rows) {
		return m, nil
	}
	r := rows[i]
	if r.kind == rowSession {
		m.quitting = true
		m.action = actionSwitch
		m.selectedID = r.sess.PaneID
		return m, tea.Quit
	}
	m.confirmOpen = true
	m.openResume = true
	m.openTarget = r.proj.Path
	m.openDisplay = r.proj.Display
	return m, nil
}

func (m model) Init() tea.Cmd {
	return tea.Batch(scan(), tick())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case scanResultMsg:
		key := m.currentKey()
		m.sessions = msg.sessions
		m.projects = msg.projects
		m.selectByKey(key)
		return m, nil

	case tickMsg:
		return m, tea.Batch(scan(), tick())

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		// While awaiting kill confirmation, capture y/n before anything else
		// so esc cancels the kill rather than quitting the TUI.
		if m.confirmKill {
			switch msg.String() {
			case "y", "Y":
				target := m.killTarget
				m.confirmKill = false
				m.killTarget = ""
				return m, killSession(target)
			default:
				m.confirmKill = false
				m.killTarget = ""
				return m, nil
			}
		}

		// Same for the open/spawn confirmation.
		if m.confirmOpen {
			switch msg.String() {
			case "y", "Y":
				m.confirmOpen = false
				m.quitting = true
				m.action = actionOpen
				m.openPath = m.openTarget
				return m, tea.Quit
			default:
				m.confirmOpen = false
				m.openTarget = ""
				m.openDisplay = ""
				m.openResume = false
				return m, nil
			}
		}

		// Search mode: printable keys edit the query; arrows/enter/esc control.
		if m.filtering {
			switch msg.Type {
			case tea.KeyCtrlC:
				m.quitting = true
				return m, tea.Quit
			case tea.KeyEsc:
				// Clearing the filter widens the list; re-anchor the cursor to
				// the row that was highlighted rather than leaving it on a
				// now-different row.
				key := m.currentKey()
				m.filtering = false
				m.filter = ""
				m.selectByKey(key)
				return m, nil
			case tea.KeyEnter:
				return m.activate(m.visibleRows(), m.cursor)
			case tea.KeyBackspace:
				m.filter = trimLastRune(m.filter)
				m.cursor = 0
				return m, nil
			case tea.KeyUp, tea.KeyCtrlP:
				m.moveCursor(m.visibleRows(), -1)
				return m, nil
			case tea.KeyDown, tea.KeyCtrlN:
				m.moveCursor(m.visibleRows(), 1)
				return m, nil
			case tea.KeySpace:
				m.filter += " "
				m.cursor = 0
				return m, nil
			case tea.KeyRunes:
				m.filter += string(msg.Runes)
				m.cursor = 0
				return m, nil
			default:
				return m, nil
			}
		}

		rows := m.visibleRows()
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			m.quitting = true
			return m, tea.Quit
		case "/":
			m.filtering = true
			m.cursor = 0
			return m, nil
		case "x":
			// Kill only applies to live sessions, not closed projects.
			if m.cursor < len(rows) && rows[m.cursor].kind == rowSession {
				m.confirmKill = true
				m.killTarget = rows[m.cursor].sess.SessionName
			}
			return m, nil
		case "n":
			// New session: spawn a *fresh* Claude in the highlighted row's
			// folder. Works on a live session too, giving a second session for
			// an already-open project (a fresh conversation, not the live one).
			if m.cursor < len(rows) {
				if dir, disp, ok := rowFolder(rows[m.cursor]); ok {
					m.confirmOpen = true
					m.openResume = false
					m.openTarget = dir
					m.openDisplay = disp
				}
			}
			return m, nil
		case "j", "down":
			m.moveCursor(rows, 1)
		case "k", "up":
			m.moveCursor(rows, -1)
		case "enter":
			return m.activate(rows, m.cursor)
		case "1", "2", "3", "4", "5", "6", "7", "8", "9":
			idx := int(msg.String()[0]-'0') - 1
			return m.activate(rows, idx)
		}
	}

	return m, nil
}

// Styles
var (
	titleStyle    = lipgloss.NewStyle().Bold(true).MarginBottom(1).MarginLeft(2)
	sectionStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).MarginLeft(2).MarginTop(1)
	filterStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).MarginLeft(2)
	selectedRow   = lipgloss.NewStyle().Background(lipgloss.Color("236"))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("242"))
	dimTitleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	projStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("111")) // soft blue
	helpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("242")).MarginTop(1).MarginLeft(2)

	statusStyles = map[int]lipgloss.Style{
		StatusWorking: lipgloss.NewStyle().Foreground(lipgloss.Color("76")),  // green
		StatusWaiting: lipgloss.NewStyle().Foreground(lipgloss.Color("214")), // amber
		StatusIdle:    lipgloss.NewStyle().Foreground(lipgloss.Color("242")), // gray
	}
)

func statusSymbol(s int) string {
	switch s {
	case StatusWorking:
		return "●"
	case StatusWaiting:
		return "◐"
	default:
		return "○"
	}
}

func statusLabel(s int) string {
	switch s {
	case StatusWorking:
		return "Working"
	case StatusWaiting:
		return "Waiting"
	default:
		return "Idle"
	}
}

// rowBudget is how many list rows fit given the terminal height, after
// reserving space for the title, help line, and (when present) the filter input
// and the "Recent projects" header. Returns len(rows) when height is unknown so
// the list isn't windowed before the first WindowSizeMsg.
func (m model) rowBudget(rows []row) int {
	if m.height <= 0 {
		return len(rows)
	}
	chrome := 5 // title (2) + help (2) + one line of slack
	if m.filtering {
		chrome++
	}
	for _, r := range rows {
		if r.kind == rowProject {
			chrome += 2 // section header + its top margin
			break
		}
	}
	if b := m.height - chrome; b >= 1 {
		return b
	}
	return 1
}

// window returns the [start, end) slice of rows to render so the cursor stays
// on screen, scrolling only when the list is taller than the budget.
func (m model) window(rows []row) (int, int) {
	n := len(rows)
	budget := m.rowBudget(rows)
	if budget >= n {
		return 0, n
	}
	start := m.cursor - budget/2
	if start < 0 {
		start = 0
	}
	if start > n-budget {
		start = n - budget
	}
	return start, start + budget
}

func (m model) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	b.WriteString(titleStyle.Render("Claude Sessions"))
	b.WriteString("\n")

	if m.filtering {
		b.WriteString(filterStyle.Render("/ " + m.filter + "▊"))
		b.WriteString("\n")
	}

	rows := m.visibleRows()

	if len(rows) == 0 {
		if strings.TrimSpace(m.filter) != "" {
			b.WriteString(dimStyle.Render("  No matches"))
		} else {
			b.WriteString(dimStyle.Render("  No Claude sessions found"))
		}
		b.WriteString("\n")
	}

	// Column widths, computed over the visible rows.
	maxSess, maxName := 0, 0
	for _, r := range rows {
		switch r.kind {
		case rowSession:
			if len(r.sess.SessionName) > maxSess {
				maxSess = len(r.sess.SessionName)
			}
		case rowProject:
			if len(r.proj.Name) > maxName {
				maxName = len(r.proj.Name)
			}
		}
	}

	start, end := m.window(rows)

	sectionShown := false
	for i := start; i < end; i++ {
		r := rows[i]
		if r.kind == rowProject && !sectionShown {
			b.WriteString(sectionStyle.Render("Recent projects"))
			b.WriteString("\n")
			sectionShown = true
		}

		pointer := "  "
		if i == m.cursor {
			pointer = " ▸"
		}
		// Only the first 9 rows are reachable by number key, and not while the
		// filter is capturing digits — so only label those.
		num := " "
		if !m.filtering && i < 9 {
			num = strconv.Itoa(i + 1)
		}

		var line string
		if r.kind == rowSession {
			s := r.sess
			style := statusStyles[s.Status]
			sym := style.Render(statusSymbol(s.Status))
			label := style.Render(fmt.Sprintf("%-7s", statusLabel(s.Status)))
			sess := fmt.Sprintf("%-*s", maxSess, s.SessionName)
			title := dimTitleStyle.Render(s.Title)
			line = fmt.Sprintf(" %s %s  %s %s   %s  %s", pointer, num, sym, label, sess, title)
		} else {
			p := r.proj
			sym := projStyle.Render("↻")
			label := projStyle.Render(fmt.Sprintf("%-7s", "Resume"))
			name := fmt.Sprintf("%-*s", maxName, p.Name)
			path := dimTitleStyle.Render(p.Display)
			line = fmt.Sprintf(" %s %s  %s %s   %s  %s", pointer, num, sym, label, name, path)
		}

		if i == m.cursor {
			line = selectedRow.Render(line)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}

	scrolled := start > 0 || end < len(rows)

	switch {
	case m.confirmKill:
		prompt := fmt.Sprintf(" kill session %q? (y/n)", m.killTarget)
		b.WriteString(statusStyles[StatusWaiting].MarginTop(1).MarginLeft(2).Render(prompt))
	case m.confirmOpen:
		verb := "open a new Claude session in"
		if m.openResume {
			verb = "resume Claude in"
		}
		prompt := fmt.Sprintf(" %s %s? (y/n)", verb, m.openDisplay)
		b.WriteString(projStyle.MarginTop(1).MarginLeft(2).Render(prompt))
	case m.filtering:
		help := " type to filter · ↑↓ select · enter open · esc clear"
		if scrolled {
			help += fmt.Sprintf("   %d/%d", m.cursor+1, len(rows))
		}
		b.WriteString(helpStyle.Render(help))
	default:
		help := " ↑↓ navigate · enter switch/resume · n new · / search · x kill · q quit"
		if scrolled {
			help += fmt.Sprintf("   %d/%d", m.cursor+1, len(rows))
		}
		b.WriteString(helpStyle.Render(help))
	}

	return b.String()
}

func main() {
	if os.Getenv("TMUX") == "" {
		fmt.Println("csm must be run inside a tmux session.")
		os.Exit(1)
	}

	p := tea.NewProgram(model{}, tea.WithAltScreen())
	result, err := p.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	final, ok := result.(model)
	if !ok {
		return
	}
	switch final.action {
	case actionSwitch:
		exec.Command("tmux", "switch-client", "-t", final.selectedID).Run()
	case actionOpen:
		openInFolder(final.openPath, final.openResume)
	}
}
