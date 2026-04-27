// Package tui provides the chat TUI for the griad client.
package tui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Run starts the chat TUI against the given orchestrator URL.
func Run(serverURL string) error {
	serverURL = strings.TrimRight(serverURL, "/")
	p := tea.NewProgram(newModel(serverURL), tea.WithAltScreen())
	_, err := p.Run()
	return err
}

// ----- styles -----

var (
	styleStatusBar = lipgloss.NewStyle().
			Background(lipgloss.Color("236")).
			Foreground(lipgloss.Color("250")).
			Padding(0, 1)
	styleStatusError = lipgloss.NewStyle().
				Background(lipgloss.Color("52")).
				Foreground(lipgloss.Color("230")).
				Padding(0, 1)
	styleHelp = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))
	styleUser = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("39"))
	styleAssistant = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("78"))
	styleSystem = lipgloss.NewStyle().
			Italic(true).
			Foreground(lipgloss.Color("241"))
)

// ----- messages -----

type workersFetchedMsg struct {
	model string
	err   error
}

type chunkMsg struct{ text string }
type doneMsg struct{ err error }

// streamCh wraps a channel so we can ship it across tea.Cmd closures
// without typing it out everywhere.
type streamCh chan tea.Msg

// ----- model -----

type message struct {
	role    string // "user" or "assistant"
	content string
}

type model struct {
	serverURL string

	history     []message
	streamingBuf string

	input    textarea.Model
	viewport viewport.Model
	spinner  spinner.Model

	width, height int
	ready         bool

	currentModel string
	statusErr    string

	streaming bool
	cancelFn  context.CancelFunc
	stream    streamCh
}

func newModel(serverURL string) model {
	ta := textarea.New()
	ta.Placeholder = "Tape ton message…  (Entrée envoie, Maj+Entrée saute une ligne)"
	ta.Prompt = "› "
	ta.CharLimit = 8192
	ta.SetHeight(3)
	ta.ShowLineNumbers = false
	ta.Focus()

	vp := viewport.New(0, 0)
	vp.SetContent(styleSystem.Render("Connexion au serveur…\n"))

	sp := spinner.New()
	sp.Spinner = spinner.MiniDot

	return model{
		serverURL: serverURL,
		input:     ta,
		viewport:  vp,
		spinner:   sp,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		textarea.Blink,
		m.spinner.Tick,
		fetchWorkers(m.serverURL),
	)
}

// ----- update -----

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// status bar (1) + viewport + input (3 + frame ~5) + help (1)
		inputH := 5
		statusH := 1
		helpH := 1
		vpH := m.height - statusH - inputH - helpH
		if vpH < 3 {
			vpH = 3
		}
		m.viewport.Width = m.width
		m.viewport.Height = vpH
		m.input.SetWidth(m.width)
		m.ready = true
		m.refreshViewport()

	case workersFetchedMsg:
		if msg.err != nil {
			m.statusErr = "serveur: " + msg.err.Error()
		} else {
			m.currentModel = msg.model
			m.statusErr = ""
			m.appendSystem(fmt.Sprintf("Connecté à %s · modèle %s", m.serverURL, msg.model))
		}

	case chunkMsg:
		m.streamingBuf += msg.text
		m.refreshViewport()
		// keep pumping
		cmds = append(cmds, waitForChunk(m.stream))

	case doneMsg:
		m.streaming = false
		if m.cancelFn != nil {
			m.cancelFn()
			m.cancelFn = nil
		}
		if msg.err != nil && !errors.Is(msg.err, context.Canceled) {
			m.appendSystem("erreur: " + msg.err.Error())
		}
		if m.streamingBuf != "" {
			m.history = append(m.history, message{role: "assistant", content: m.streamingBuf})
		}
		m.streamingBuf = ""
		m.refreshViewport()

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		cmds = append(cmds, cmd)

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			if m.streaming && m.cancelFn != nil {
				m.cancelFn()
			}
			return m, tea.Quit
		case "esc":
			if m.streaming && m.cancelFn != nil {
				m.cancelFn()
				m.appendSystem("génération annulée")
			}
		case "enter":
			if !m.streaming && m.input.Value() != "" {
				text := strings.TrimSpace(m.input.Value())
				if text == "" {
					m.input.Reset()
					break
				}
				m.input.Reset()
				m.history = append(m.history, message{role: "user", content: text})
				if m.currentModel == "" {
					m.appendSystem("aucun worker disponible — réessaie plus tard")
					m.refreshViewport()
					break
				}
				m.streaming = true
				m.streamingBuf = ""
				ctx, cancel := context.WithCancel(context.Background())
				m.cancelFn = cancel
				ch := make(streamCh, 64)
				m.stream = ch
				go runStream(ctx, ch, m.serverURL, m.history)
				m.refreshViewport()
				cmds = append(cmds, waitForChunk(ch))
			}
		default:
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			cmds = append(cmds, cmd)
		}

	default:
		// pass-through to children that need ticks etc.
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		cmds = append(cmds, cmd)
		m.viewport, cmd = m.viewport.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

// ----- view -----

func (m model) View() string {
	if !m.ready {
		return "démarrage…"
	}

	status := m.statusBar()
	help := m.helpLine()

	return strings.Join([]string{
		status,
		m.viewport.View(),
		m.input.View(),
		help,
	}, "\n")
}

func (m model) statusBar() string {
	if m.statusErr != "" {
		return styleStatusError.Width(m.width).Render(m.statusErr)
	}
	left := "TheGriad"
	right := m.serverURL
	if m.currentModel != "" {
		right = m.currentModel + " · " + m.serverURL
	}
	if m.streaming {
		right = m.spinner.View() + " génération · " + right
	}
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 1 {
		gap = 1
	}
	return styleStatusBar.Width(m.width).Render(left + strings.Repeat(" ", gap) + right)
}

func (m model) helpLine() string {
	hint := "Entrée: envoyer · Maj+Entrée: saut de ligne · Esc: annuler · Ctrl+C: quitter"
	return styleHelp.Render(hint)
}

func (m *model) refreshViewport() {
	var b strings.Builder
	for _, msg := range m.history {
		switch msg.role {
		case "user":
			b.WriteString(styleUser.Render("Toi"))
			b.WriteString("\n")
			b.WriteString(msg.content)
			b.WriteString("\n\n")
		case "assistant":
			b.WriteString(styleAssistant.Render("Assistant"))
			b.WriteString("\n")
			b.WriteString(msg.content)
			b.WriteString("\n\n")
		case "system":
			b.WriteString(styleSystem.Render(msg.content))
			b.WriteString("\n\n")
		}
	}
	if m.streaming {
		b.WriteString(styleAssistant.Render("Assistant"))
		b.WriteString("\n")
		b.WriteString(m.streamingBuf)
		if m.streamingBuf == "" {
			b.WriteString(styleSystem.Render("…"))
		}
		b.WriteString("\n")
	}
	m.viewport.SetContent(b.String())
	m.viewport.GotoBottom()
}

func (m *model) appendSystem(text string) {
	m.history = append(m.history, message{role: "system", content: text})
}

// ----- commands & streaming -----

func fetchWorkers(serverURL string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, serverURL+"/api/workers", nil)
		if err != nil {
			return workersFetchedMsg{err: err}
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return workersFetchedMsg{err: err}
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return workersFetchedMsg{err: fmt.Errorf("statut %d", resp.StatusCode)}
		}
		var workers []struct {
			LoadedModel string `json:"LoadedModel"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&workers); err != nil {
			return workersFetchedMsg{err: err}
		}
		for _, w := range workers {
			if w.LoadedModel != "" {
				return workersFetchedMsg{model: w.LoadedModel}
			}
		}
		return workersFetchedMsg{err: errors.New("aucun worker avec un modèle chargé")}
	}
}

func waitForChunk(ch streamCh) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return doneMsg{}
		}
		return msg
	}
}

type chatBody struct {
	Model     string              `json:"model"`
	Messages  []map[string]string `json:"messages"`
	MaxTokens int                 `json:"max_tokens"`
	Stream    bool                `json:"stream"`
}

func runStream(ctx context.Context, out streamCh, serverURL string, hist []message) {
	defer close(out)

	body := chatBody{
		Model:     "default",
		Stream:    true,
		MaxTokens: 512,
	}
	for _, m := range hist {
		if m.role == "system" {
			continue
		}
		body.Messages = append(body.Messages, map[string]string{
			"role":    m.role,
			"content": m.content,
		})
	}
	raw, err := json.Marshal(body)
	if err != nil {
		out <- doneMsg{err: err}
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		out <- doneMsg{err: err}
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		out <- doneMsg{err: err}
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		out <- doneMsg{err: fmt.Errorf("orchestrateur %d: %s", resp.StatusCode, string(buf))}
		return
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			out <- doneMsg{}
			return
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		if ev.Error != nil {
			out <- doneMsg{err: errors.New(ev.Error.Message)}
			return
		}
		for _, c := range ev.Choices {
			if c.Delta.Content != "" {
				out <- chunkMsg{text: c.Delta.Content}
			}
			if c.FinishReason != nil && *c.FinishReason != "" {
				out <- doneMsg{}
				return
			}
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, context.Canceled) {
		out <- doneMsg{err: err}
		return
	}
	out <- doneMsg{}
}
