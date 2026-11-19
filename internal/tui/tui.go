// Package tui implements the embedded terminal setup and live status screen.
package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"scanspool/internal/config"
	"scanspool/internal/service"
	"scanspool/internal/setup"
	"scanspool/internal/vens"
)

type statusMsg struct {
	value service.Status
	err   error
}
type resultMsg struct {
	kind  string
	value any
	err   error
}
type tickMsg time.Time
type field struct {
	label, value   string
	secret, toggle bool
	choices        []string
}
type model struct {
	api                      service.API
	ctx                      context.Context
	status                   service.Status
	settings                 config.View
	profile                  config.ProfileView
	version                  string
	mode, message            string
	fields                   []field
	candidates               []setup.Candidate
	selected                 setup.Candidate
	cursor, width, height    int
	working, quit, cancelled bool
	cancel                   context.CancelFunc
}

var menu = []string{"Scanner status", "Set up / change scanner", "Output destination", "Retry queued deliveries", "Recover previous registration", "Start scan", "Scan profile", "Quit"}

const noScannersFoundMessage = "No scanners found."

func Run(ctx context.Context, api service.API, version string) error {
	m := &model{api: api, ctx: ctx, version: version, mode: "home", width: 80, height: 24}
	final, err := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if final.(*model).cancelled {
		return context.Canceled
	}
	return nil
}
func (m *model) Init() tea.Cmd { return tea.Batch(m.refresh(), tick()) }
func tick() tea.Cmd            { return tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) }) }
func (m *model) refresh() tea.Cmd {
	return func() tea.Msg { v, e := m.api.Status(m.ctx); return statusMsg{v, e} }
}
func (m *model) start(kind string, fn func(context.Context) (any, error)) tea.Cmd {
	m.working = true
	m.message = ""
	ctx, cancel := context.WithTimeout(m.ctx, 2*time.Minute)
	m.cancel = cancel
	return func() tea.Msg { defer cancel(); v, e := fn(ctx); return resultMsg{kind, v, e} }
}
func (m *model) openFTP(view config.View) {
	m.settings = view
	m.mode = "ftp"
	m.cursor = 0
	m.fields = []field{{label: "FTP host[:port]", value: view.FTP.Host}, {label: "FTP user", value: view.FTP.User}, {label: "New password (blank = keep)", secret: true}, {label: "Use empty password", value: "false", toggle: true}, {label: "FTP directory", value: view.FTP.Directory}}
}
func (m *model) openDestination(view config.View) {
	m.settings = view
	m.mode = "destination"
	m.fields = nil
	m.cursor = 0
	if view.Kind == config.KindLocal {
		m.cursor = 1
	}
}
func (m *model) openProfile(view config.ProfileView) {
	m.profile = view
	m.mode = "profile"
	m.cursor = 0
	m.fields = nil
	for _, option := range config.ScanOptions() {
		m.fields = append(m.fields, field{label: option.Label, value: option.Value(view.Profile), choices: option.Choices})
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = value.Width
		m.height = value.Height
	case tickMsg:
		return m, tea.Batch(m.refresh(), tick())
	case statusMsg:
		if value.err != nil {
			if !m.working {
				m.message = value.err.Error()
			}
		} else {
			m.status = value.value
		}
	case resultMsg:
		m.working = false
		m.cancel = nil
		if m.quit {
			return m, tea.Quit
		}
		if value.err != nil {
			m.message = value.err.Error()
			if errors.Is(value.err, config.ErrConflict) {
				m.message = "Settings changed in another session. Esc, then reopen this form."
			}
			return m, m.refresh()
		}
		switch value.kind {
		case "profile":
			m.openProfile(value.value.(config.ProfileView))
			return m, nil
		case "saveProfile":
			m.mode = "home"
			m.fields = nil
			m.message = "Scan profile saved. It applies to the next scan."
		case "settings":
			m.openDestination(value.value.(config.View))
			return m, nil
		case "discover":
			m.candidates = value.value.([]setup.Candidate)
			m.cursor = 0
			if len(m.candidates) == 0 {
				m.message = noScannersFoundMessage
			} else {
				m.mode = "candidates"
			}
		case "connect":
			m.mode = "home"
			m.fields = nil
			m.cursor = 0
			m.message = "Scanner registration saved. Configure the output destination next."
		case "save":
			m.mode = "home"
			m.fields = nil
			m.cursor = 0
			m.message = "Output destination saved."
		case "retry":
			m.mode = "home"
			m.message = "Matching queued deliveries processed; other targets remain queued."
		case "scan":
			m.message = "Scan started. Check Scanner status for progress and errors."
		case "recover":
			m.mode = "home"
			m.fields = nil
			m.message = "Previous scanner registration restored."
		}
		return m, m.refresh()
	case tea.PasteMsg:
		if !m.working {
			m.append(value.Content)
		}
	case tea.KeyPressMsg:
		key := value.String()
		if key == "ctrl+c" {
			m.cancelled = true
			if m.working {
				m.quit = true
				if m.cancel != nil {
					m.cancel()
				}
				return m, nil
			}
			return m, tea.Quit
		}
		if m.working {
			if key == "esc" && m.cancel != nil {
				m.cancel()
			}
			return m, nil
		}
		if key == "esc" {
			m.mode = "home"
			m.fields = nil
			m.cursor = 0
			m.message = "Draft discarded."
			return m, nil
		}
		if m.mode == "home" {
			switch key {
			case "q":
				return m, tea.Quit
			case "up", "k":
				m.cursor = (m.cursor + len(menu) - 1) % len(menu)
			case "down", "j":
				m.cursor = (m.cursor + 1) % len(menu)
			case "enter":
				switch m.cursor {
				case 0:
					m.mode = "status"
					m.message = ""
				case 1:
					m.mode = "discover"
					m.cursor = 0
					m.fields = []field{{label: "Scanner IPv4 (blank = discover)"}}
				case 2:
					return m, m.start("settings", func(ctx context.Context) (any, error) { return m.api.Settings(ctx) })
				case 3:
					return m, m.start("retry", func(ctx context.Context) (any, error) { return nil, m.api.Retry(ctx) })
				case 4:
					m.mode = "recover"
					m.cursor = 0
					m.fields = []field{{label: "Confirm restoring previous registration", value: "false", toggle: true}}
				case 5:
					return m, m.start("scan", func(ctx context.Context) (any, error) { return nil, m.api.StartScan(ctx) })
				case 6:
					return m, m.start("profile", func(ctx context.Context) (any, error) { return m.api.Profile(ctx) })
				case 7:
					return m, tea.Quit
				}
			}
			return m, nil
		}
		if m.mode == "status" {
			if key == "enter" {
				m.mode = "home"
			}
			return m, nil
		}
		if m.mode == "destination" {
			switch key {
			case "up", "down", "tab", "shift+tab", "left", "right", "space":
				m.cursor = 1 - m.cursor
			case "enter":
				if m.cursor == 0 {
					m.openFTP(m.settings)
				} else {
					m.mode = "local"
					m.cursor = 0
					m.fields = []field{{label: "Local directory (absolute path)", value: m.settings.LocalDirectory}}
				}
			}
			return m, nil
		}
		if m.mode == "candidates" {
			switch key {
			case "up":
				m.cursor = (m.cursor + len(m.candidates) - 1) % len(m.candidates)
			case "down":
				m.cursor = (m.cursor + 1) % len(m.candidates)
			case "enter":
				candidate := m.candidates[m.cursor]
				if !candidate.Supported {
					m.message = "This release supports iX1300 interface version 16."
					return m, nil
				}
				m.selected = candidate
				m.mode = "register"
				m.cursor = 0
				m.fields = []field{{label: "Scanner connection password", secret: true}, {label: "Replace current computer registration", value: "false", toggle: true}}
			}
			return m, nil
		}
		if len(m.fields) == 0 {
			return m, nil
		}
		f := &m.fields[m.cursor]
		switch key {
		case "up", "shift+tab":
			m.cursor = (m.cursor + len(m.fields) - 1) % len(m.fields)
		case "down", "tab":
			m.cursor = (m.cursor + 1) % len(m.fields)
		case "left", "right", "space":
			if len(f.choices) > 0 {
				step := 1
				if key == "left" {
					step = -1
				}
				for i, choice := range f.choices {
					if f.value == choice {
						f.value = f.choices[(i+step+len(f.choices))%len(f.choices)]
						break
					}
				}
			} else if f.toggle {
				if f.value == "true" {
					f.value = "false"
				} else {
					f.value = "true"
				}
			} else if key == "space" {
				m.append(" ")
			}
		case "ctrl+r":
			if m.mode == "profile" {
				options := config.ScanOptions()
				for i := range m.fields {
					m.fields[i].value = options[i].Value(vens.PCReferenceConfig())
				}
				m.message = "Defaults loaded. Enter saves, Esc discards."
			}
		case "ctrl+u":
			if !f.toggle && len(f.choices) == 0 {
				f.value = ""
			}
		case "backspace":
			if !f.toggle && len(f.choices) == 0 && f.value != "" {
				_, n := utf8.DecodeLastRuneInString(f.value)
				f.value = f.value[:len(f.value)-n]
			}
		case "enter":
			return m, m.apply()
		default:
			if value.Text != "" {
				m.append(value.Text)
			}
		}
	}
	return m, nil
}
func (m *model) append(text string) {
	if len(m.fields) == 0 || m.cursor >= len(m.fields) {
		return
	}
	f := &m.fields[m.cursor]
	if f.toggle || len(f.choices) > 0 {
		return
	}
	text = clean(text)
	if len(f.value)+len(text) <= 4096 {
		f.value += text
	}
}
func (m *model) apply() tea.Cmd {
	switch m.mode {
	case "profile":
		update := m.profile
		for i, option := range config.ScanOptions() {
			if err := option.Set(&update.Profile, m.fields[i].value); err != nil {
				m.message = err.Error()
				return nil
			}
		}
		return m.start("saveProfile", func(ctx context.Context) (any, error) { return m.api.SaveProfile(ctx, update) })
	case "discover":
		ip := m.fields[0].value
		return m.start("discover", func(ctx context.Context) (any, error) { return m.api.Discover(ctx, ip) })
	case "register":
		if m.fields[1].value != "true" {
			m.message = "Explicitly confirm replacement before registering."
			return nil
		}
		request := setup.Request{IP: m.selected.IP, Serial: m.selected.Serial, Password: m.fields[0].value, ConfirmReplacement: true}
		return m.start("connect", func(ctx context.Context) (any, error) { return nil, m.api.Connect(ctx, request) })
	case "ftp":
		request := config.Update{Kind: config.KindFTP, Revision: m.settings.Revision, FTP: config.FTP{Host: m.fields[0].value, User: m.fields[1].value, Directory: m.fields[4].value}, PasswordAction: "keep"}
		if m.fields[2].value != "" || m.fields[3].value == "true" {
			request.PasswordAction = "set"
			request.FTP.Password = m.fields[2].value
			if m.fields[3].value == "true" {
				request.FTP.Password = ""
			}
		}
		return m.start("save", func(ctx context.Context) (any, error) { return m.api.Save(ctx, request) })
	case "local":
		request := config.Update{Kind: config.KindLocal, LocalDirectory: m.fields[0].value, Revision: m.settings.Revision}
		return m.start("save", func(ctx context.Context) (any, error) { return m.api.Save(ctx, request) })
	case "recover":
		if m.fields[0].value != "true" {
			m.message = "Explicit confirmation required."
			return nil
		}
		return m.start("recover", func(ctx context.Context) (any, error) { return nil, m.api.Recover(ctx) })
	}
	return nil
}
func clean(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, ansi.Strip(value))
}

func statusValue(value, fallback string) string {
	value = clean(value)
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// Diagnostics must remain readable even when filenames or paths are wider
// than the terminal. Only plain text is sliced after ANSI/control sanitization.
func diagnosticLines(prefix, value string, width int) []string {
	var lines []string
	for index, paragraph := range strings.Split(value, "\n") {
		remaining := clean(paragraph)
		if index == 0 {
			remaining = prefix + remaining
		}
		if remaining == "" {
			lines = append(lines, "")
		}
		for remaining != "" {
			line := ansi.Truncate(remaining, width, "")
			if line == "" {
				break
			}
			lines = append(lines, line)
			remaining = remaining[len(line):]
		}
	}
	return lines
}

func (m *model) View() tea.View {
	if m.width < 60 || m.height < 18 {
		v := tea.NewView("Enlarge terminal (at least 60 x 18). Ctrl+C to quit.")
		v.AltScreen = true
		return v
	}
	state := "not configured"
	if m.status.Configured {
		state = "offline / connecting"
	}
	if m.status.Online {
		state = "online"
	}
	title := "ScanSpool"
	version := clean(m.version)
	if version != "" {
		title += " " + version
	}
	lines := []string{title + " · " + m.mode}
	if receiver := clean(m.status.Version); m.status.Running && receiver != "" && receiver != version {
		lines = append(lines, "Receiver version: "+receiver)
	}
	lines = append(lines,
		"Scanner: "+state+"  "+clean(m.status.Host),
		fmt.Sprintf("Output: %s · configured: %t", clean(m.status.OutputKind), m.status.OutputReady),
	)
	if m.mode == "home" && m.status.OutputDirectory != "" {
		lines = append(lines, "Output directory: "+clean(m.status.OutputDirectory))
	}
	lines = append(lines, fmt.Sprintf("Queued PDFs: %d · other targets: %d · busy: %t", m.status.Pending, m.status.OtherTargetPending, m.status.Busy), "")
	if m.status.RecoveryRequired {
		lines = append(lines, "! Registration recovery required")
	}
	if m.mode == "home" && m.status.Error != "" {
		hint := statusValue(m.status.ErrorHint, "Scan or delivery failed. Open Scanner status for details.")
		// Keep navigation visible; detailed repair instructions live in status.
		if strings.Contains(m.status.ErrorHint, "\n") {
			hint = strings.SplitN(m.status.ErrorHint, "\n", 2)[0]
		}
		lines = append(lines, diagnosticLines("! ", hint, m.width)...)
		if strings.Contains(m.status.ErrorHint, "\nRun on receiver") {
			lines = append(lines, "! Repair command: open Scanner status.")
		}
		if m.status.Pending > 0 {
			lines = append(lines, fmt.Sprintf("! %d PDF(s) in outbox; fix error, then retry delivery.", m.status.Pending))
		}
		lines = append(lines, "")
	}
	switch m.mode {
	case "destination":
		for i, label := range []string{"FTP", "Local folder"} {
			prefix := "  "
			if i == m.cursor {
				prefix = "> "
			}
			lines = append(lines, prefix+label)
		}
	case "home":
		visible := max(1, m.height-len(lines)-3)
		first := max(0, m.cursor-visible+1)
		last := min(len(menu), first+visible)
		for i := first; i < last; i++ {
			prefix := "  "
			if i == m.cursor {
				prefix = "> "
			}
			lines = append(lines, prefix+menu[i])
		}
	case "status":
		if m.status.Error != "" {
			if m.status.ErrorHint != "" {
				lines = append(lines, diagnosticLines("! ", m.status.ErrorHint, m.width)...)
			}
			lines = append(lines, diagnosticLines("Last job error: ", m.status.Error, m.width)...)
		}
		if !m.status.Configured {
			lines = append(lines,
				"Scanner setup has not been completed.",
				"Choose 'Set up / change scanner' from the main menu.",
				"Discovering a scanner does not save its registration.")
		} else {
			paper, wifi := "unknown", "unavailable (offline)"
			if m.status.PaperLoaded != nil {
				paper = fmt.Sprintf("%t", *m.status.PaperLoaded)
			}
			if m.status.Online {
				wifi = fmt.Sprintf("%d", m.status.WiFi)
			}
			lines = append(lines,
				"Model: "+statusValue(m.status.Model, "unknown"),
				"Serial: "+statusValue(m.status.Serial, "unknown"),
				"Firmware: "+statusValue(m.status.Firmware, "unknown"),
				fmt.Sprintf("Wi-Fi level: %s · paper loaded: %s", wifi, paper))
		}
		lines = append(lines, "Service/session: "+statusValue(m.status.Mode, "unknown"))
		if m.status.LastScan == "" {
			lines = append(lines, "Last scan: none yet")
		} else {
			lines = append(lines, fmt.Sprintf("Last scan: %s · pages: %d", clean(m.status.LastScan), m.status.LastPages))
		}
		lines = append(lines,
			"Last file: "+statusValue(m.status.LastFile, "none yet"),
			"Output directory: "+statusValue(m.status.OutputDirectory, "not configured"))
		if m.status.ScannerError != "" {
			lines = append(lines, "Scanner error: "+clean(m.status.ScannerError))
		}
	case "candidates":
		visible := max(1, m.height-8)
		first := max(0, m.cursor-visible+1)
		last := min(len(m.candidates), first+visible)
		for i := first; i < last; i++ {
			c := m.candidates[i]
			prefix := "  "
			if i == m.cursor {
				prefix = "> "
			}
			label := c.Name + " · " + c.IP + " · " + c.Serial
			if !c.Supported {
				label += " [unsupported]"
			}
			if c.InUse {
				label += " [in use]"
			}
			lines = append(lines, prefix+clean(label))
		}
	default:
		if m.mode == "register" {
			lines = append(lines, clean(m.selected.Name+" · "+m.selected.Serial))
		}
		first, last := 0, len(m.fields)
		if m.mode == "profile" {
			lines = append(lines, "Custom profiles: hardware verification pending.")
			messageLines := 0
			if m.working {
				messageLines = 1
			} else if m.message != "" {
				messageLines = len(diagnosticLines("", m.message, m.width))
			}
			visible := max(1, m.height-len(lines)-3-messageLines)
			first = max(0, m.cursor-visible+1)
			last = min(len(m.fields), first+visible)
		}
		for i := first; i < last; i++ {
			f := m.fields[i]
			prefix := "  "
			if i == m.cursor {
				prefix = "> "
			}
			value := f.value
			if f.secret {
				value = strings.Repeat("*", utf8.RuneCountInString(value))
				if value == "" && m.mode == "ftp" && m.settings.HasPassword {
					value = "(keep existing)"
				}
			}
			lines = append(lines, prefix+f.label+": "+clean(value))
		}
		if m.mode == "local" {
			lines = append(lines, "\x1b[3mFolder is created and write-tested when saved.\x1b[23m")
		}
	}
	lines = append(lines, "")
	if m.working {
		lines = append(lines, "Working… Esc cancels; cleanup finishes before exit.")
	} else if m.message != "" {
		lines = append(lines, diagnosticLines("", m.message, m.width)...)
		if m.mode == "discover" && m.message == noScannersFoundMessage {
			lines = append(lines,
				"\x1b[3mSwitch scanner on and wait for its Wi-Fi connection.\x1b[23m",
				"\x1b[3mThen press Enter to retry, or enter its IPv4 address.\x1b[23m")
		}
	}
	if m.status.Error != "" && m.mode != "status" && m.mode != "home" && m.mode != "profile" && m.message == "" {
		lines = append(lines, diagnosticLines("! ", statusValue(m.status.ErrorHint, "Scan or delivery failed. Open Scanner status for details."), m.width)...)
	}
	if m.mode == "profile" {
		lines = append(lines, "↑↓ Move · ←→ Choose · Type numbers · Ctrl+R Defaults", "Enter Save · Esc Discard")
	} else {
		lines = append(lines, "Arrows/Tab move · Enter applies · Space toggles · Esc discards · Ctrl+C quits")
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], m.width, "…")
	}
	if len(lines) > m.height {
		lines = append(lines[:m.height-2], lines[len(lines)-2:]...)
	}
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	return view
}
