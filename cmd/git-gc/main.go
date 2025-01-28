package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/ugurcsen/gods-generic/sets/hashset"
)

type model struct {
	directories []string
	width       int
	height      int

	spinner  spinner.Model
	progress progress.Model

	done        bool
	concurrency int // user-supplied concurrency
	inFlight    int // how many GCs are currently running
	nextIndex   int // which dir to spawn next
	index       int // how many GCs completed

	styles styles
}

type styles struct {
	checkmark      lipgloss.Style
	done           lipgloss.Style
	currentDirName lipgloss.Style
}

type dirGitGCCompleted string

func main() {
	var (
		rootDir  string
		parallel int
	)
	flag.StringVar(&rootDir, "root", "", "Root directory to search for git repos")
	flag.IntVar(&parallel, "parallel", runtime.NumCPU(), "Number of parallel git gc processes to run")

	m, err := newModel(rootDir, parallel)
	if err != nil {
		fmt.Println("Error creating new model:", err)
		os.Exit(1)
	}

	if _, err := tea.NewProgram(m).Run(); err != nil {
		fmt.Println("Error running program:", err)
		os.Exit(1)
	}
}

func (m model) Init() tea.Cmd {
	spinnerCmd := m.spinner.Tick

	// If no directories at all, just quit
	if len(m.directories) == 0 {
		return tea.Sequence(
			spinnerCmd,
			tea.Quit,
		)
	}

	// Start up to “concurrency” processes
	toSpawn := min(m.concurrency, len(m.directories))
	initialCmds := make([]tea.Cmd, toSpawn)
	for i := range toSpawn {
		initialCmds[i] = runGitGC(m.directories[m.nextIndex])
		m.nextIndex++
		m.inFlight++
	}

	// Return a batch of all initial GCs plus the spinner
	return tea.Batch(
		spinnerCmd,
		tea.Batch(initialCmds...),
	)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc", "q":
			return m, tea.Quit
		}
	case dirGitGCCompleted:
		// `msg` is the directory that just finished
		pkg := string(msg)

		m.index++
		m.inFlight--

		// Update our progress bar
		progressCmd := m.progress.SetPercent(
			float64(m.index) / float64(len(m.directories)),
		)
		// Print checkmark for the completed directory
		checkMarkCmd := tea.Printf("%s %s", m.styles.checkmark, pkg)

		// If we still have more directories, spawn another
		var nextCmd tea.Cmd
		if m.nextIndex < len(m.directories) {
			nextCmd = runGitGC(m.directories[m.nextIndex])
			m.nextIndex++
			m.inFlight++
		}

		// If *all* directories have finished, we’re done
		if m.index >= len(m.directories) {
			m.done = true
			return m, tea.Batch(progressCmd, checkMarkCmd, tea.Quit)
		}

		// Otherwise, continue
		return m, tea.Batch(progressCmd, checkMarkCmd, nextCmd)
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case progress.FrameMsg:
		newModel, cmd := m.progress.Update(msg)
		if newProg, ok := newModel.(progress.Model); ok {
			m.progress = newProg
		}
		return m, cmd
	case tea.QuitMsg:
		return m, tea.Quit
	}

	return m, nil
}

func (m model) View() string {
	total := len(m.directories)
	if m.done {
		return m.styles.done.Render(
			fmt.Sprintf("Done! Ran garbage collection on %d repos.\n", total),
		)
	}

	var (
		spin = m.spinner.View() + " "
		prog = m.progress.View()

		// compute spacing based on terminal width
		pkgCount = fmt.Sprintf(" %d/%d", m.index, total)
		info     = lipgloss.NewStyle().
				MaxWidth(max(0, m.width-lipgloss.Width(spin+prog+pkgCount))).
				Render(fmt.Sprintf("Cleaning repos... %d/%d complete", m.index, total))
	)

	return spin +
		info +
		strings.Repeat(" ", max(0, m.width-lipgloss.Width(spin+info+prog+pkgCount))) +
		prog +
		pkgCount
}

func newModel(rootDir string, concurrency int) (model, error) {
	s := spinner.New()
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("63"))

	dirs, err := findDirectories(rootDir)
	if err != nil {
		return model{}, err
	}

	return model{
		directories: dirs,
		concurrency: concurrency,
		spinner:     s,
		progress: progress.New(
			progress.WithDefaultGradient(),
			progress.WithWidth(40),
			progress.WithoutPercentage(),
		),
		styles: newStyles(),
	}, nil
}

func newStyles() styles {
	return styles{
		checkmark:      lipgloss.NewStyle().Foreground(lipgloss.Color("42")).SetString("✓"),
		done:           lipgloss.NewStyle().Margin(1, 2),
		currentDirName: lipgloss.NewStyle().Foreground(lipgloss.Color("211")),
	}
}

func findDirectories(rootDir string) ([]string, error) {
	if rootDir == "" {
		var err error
		rootDir, err = os.UserHomeDir()
		if err != nil {
			return nil, err
		}
	}

	root, err := filepath.Abs(os.ExpandEnv(rootDir))
	if err != nil {
		return nil, err
	}

	fi, err := os.Stat(root)
	if err != nil {
		return nil, err
	}

	if !fi.IsDir() {
		return nil, errors.New("root dir '" + root + "' is not a directory")
	}

	dirs := hashset.New[string]()
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() && !strings.HasPrefix(info.Name(), ".") {
			if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
				dirs.Add(path)
			}
		}

		return nil
	}); err != nil {
		return nil, err
	}

	dirsSlice := dirs.Values()
	slices.Sort(dirsSlice)
	return dirsSlice, nil
}

func runGitGC(dir string) tea.Cmd {
	cmd := exec.Command("git", "-C", dir, "gc")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	return tea.ExecProcess(cmd, func(exitErr error) tea.Msg {
		if exitErr != nil {
			return tea.Quit
		}

		return dirGitGCCompleted(dir)
	})
}
