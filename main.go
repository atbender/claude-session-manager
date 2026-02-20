package main

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
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

type ClaudeSession struct {
	PaneID      string
	SessionName string
	Title       string
	Path        string
	Status      int
}

// Messages
type sessionsMsg []ClaudeSession
type tickMsg time.Time

// Commands
func scan() tea.Cmd {
	return func() tea.Msg {
		sessions := detectSessions()
		return sessionsMsg(sessions)
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}


// Detection pipeline

// shellCommands lists processes that indicate Claude has exited.
var shellCommands = map[string]bool{
	"zsh": true, "bash": true, "fish": true, "sh": true, "dash": true,
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

func detectSessions() []ClaudeSession {
	// Step 1: list all panes (includes pane_current_command for liveness check)
	out, err := exec.Command("tmux", "list-panes", "-a", "-F",
		"#{session_name}:#{window_index}.#{pane_index}\t#{pane_current_path}\t#{pane_title}\t#{pane_current_command}").Output()
	if err != nil {
		return nil
	}

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
		cmd := parts[3]

		// Check A: title must start with ✳ or Braille spinner
		if !isClaudeTitle(title) {
			continue
		}
		// Check B: command must not be a shell (indicates Claude has exited)
		if shellCommands[cmd] {
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
		return nil
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
				Status:      status,
			}
			valid[idx] = true
		}(i, c)
	}
	wg.Wait()

	var sessions []ClaudeSession
	for i, v := range valid {
		if v {
			sessions = append(sessions, results[i])
		}
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].PaneID < sessions[j].PaneID
	})

	return sessions
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

// Bubble Tea model

type model struct {
	sessions   []ClaudeSession
	cursor     int
	width      int
	height     int
	quitting   bool
	selectedID string
}

func (m model) Init() tea.Cmd {
	return tea.Batch(scan(), tick())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case sessionsMsg:
		oldID := ""
		if m.cursor < len(m.sessions) {
			oldID = m.sessions[m.cursor].PaneID
		}
		m.sessions = msg
		// Preserve cursor position by matching PaneID
		if oldID != "" {
			for i, s := range m.sessions {
				if s.PaneID == oldID {
					m.cursor = i
					return m, nil
				}
			}
		}
		if m.cursor >= len(m.sessions) {
			m.cursor = max(0, len(m.sessions)-1)
		}
		return m, nil

	case tickMsg:
		return m, tea.Batch(scan(), tick())

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			m.quitting = true
			return m, tea.Quit
		case "j", "down":
			if len(m.sessions) > 0 {
				m.cursor = (m.cursor + 1) % len(m.sessions)
			}
		case "k", "up":
			if len(m.sessions) > 0 {
				m.cursor = (m.cursor - 1 + len(m.sessions)) % len(m.sessions)
			}
		case "enter":
			if m.cursor < len(m.sessions) {
				m.quitting = true
				m.selectedID = m.sessions[m.cursor].PaneID
				return m, tea.Quit
			}
		case "1", "2", "3", "4", "5", "6", "7", "8", "9":
			idx := int(msg.String()[0]-'0') - 1
			if idx < len(m.sessions) {
				m.quitting = true
				m.selectedID = m.sessions[idx].PaneID
				return m, tea.Quit
			}
		}
	}

	return m, nil
}

// Styles
var (
	titleStyle    = lipgloss.NewStyle().Bold(true).MarginBottom(1).MarginLeft(2)
	selectedRow   = lipgloss.NewStyle().Background(lipgloss.Color("236"))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("242"))
	dimTitleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
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

func (m model) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	b.WriteString(titleStyle.Render("Claude Sessions"))
	b.WriteString("\n")

	if len(m.sessions) == 0 {
		b.WriteString(dimStyle.Render("  No Claude sessions found"))
		b.WriteString("\n")
	} else {
		// Calculate column widths
		maxSess := 0
		for _, s := range m.sessions {
			if len(s.SessionName) > maxSess {
				maxSess = len(s.SessionName)
			}
		}

		for i, s := range m.sessions {
			pointer := "  "
			if i == m.cursor {
				pointer = " ▸"
			}

			style := statusStyles[s.Status]
			sym := style.Render(statusSymbol(s.Status))
			label := style.Render(fmt.Sprintf("%-7s", statusLabel(s.Status)))

			num := fmt.Sprintf("%d", i+1)
			sess := fmt.Sprintf("%-*s", maxSess, s.SessionName)
			title := dimTitleStyle.Render(s.Title)

			line := fmt.Sprintf(" %s %s  %s %s   %s  %s", pointer, num, sym, label, sess, title)

			if i == m.cursor {
				line = selectedRow.Render(line)
			}

			b.WriteString(line)
			b.WriteString("\n")
		}
	}

	b.WriteString(helpStyle.Render(" ↑↓ navigate · enter switch · q quit"))

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
	if final, ok := result.(model); ok && final.selectedID != "" {
		exec.Command("tmux", "switch-client", "-t", final.selectedID).Run()
	}
}
