/*
Copyright (C) 2021-2023, Kubefirst

This program is licensed under MIT.
See the LICENSE file for more details.
*/
package k3d

import (
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	ListHeight   = 14
	DefaultWidth = 20
)

var (
	TitleStyle        = lipgloss.NewStyle().MarginLeft(2)
	ItemStyle         = lipgloss.NewStyle().PaddingLeft(4)
	SelectedItemStyle = lipgloss.NewStyle().PaddingLeft(2).Foreground(lipgloss.Color("170"))
	PaginationStyle   = list.DefaultStyles().PaginationStyle.PaddingLeft(4)
	HelpStyle         = list.DefaultStyles().HelpStyle.PaddingLeft(4).PaddingBottom(1)
	QuitTextStyle     = lipgloss.NewStyle().Margin(1, 0, 2, 4)
)

type Item string

func (i Item) FilterValue() string { return "" }

type ItemDelegate struct{}

func (d ItemDelegate) Height() int                             { return 1 }
func (d ItemDelegate) Spacing() int                            { return 0 }
func (d ItemDelegate) Update(_ tea.Msg, _ *list.Model) tea.Cmd { return nil }
func (d ItemDelegate) Render(w io.Writer, m list.Model, index int, listItem list.Item) {
	i, ok := listItem.(Item)
	if !ok {
		return
	}

	str := fmt.Sprintf("%d. %s", index+1, i)

	fn := ItemStyle.Render
	if index == m.Index() {
		fn = func(s ...string) string {
			return SelectedItemStyle.Render("> " + strings.Join(s, " "))
		}
	}

	fmt.Fprint(w, fn(str))
}

type Model struct {
	List     list.Model
	Choice   string
	Quitting bool
}

func (m Model) Init() tea.Cmd {
	return nil
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.List.SetWidth(msg.Width)
		return m, nil

	case tea.KeyMsg:
		switch keypress := msg.String(); keypress {
		case "ctrl+c":
			m.Quitting = true
			return m, tea.Quit

		case "enter":
			i, ok := m.List.SelectedItem().(Item)
			if ok {
				m.Choice = string(i)
			}
			return m, tea.Quit
		}
	}

	var cmd tea.Cmd
	m.List, cmd = m.List.Update(msg)
	return m, cmd
}

func (m Model) View() string {
	if m.Choice != "" {
		return QuitTextStyle.Render(m.Choice)
	}
	if m.Quitting {
		return QuitTextStyle.Render("Quitting.")
	}
	return "\n" + m.List.View()
}

func MongoDestinationChooser(inCluster bool) (string, error) {
	if inCluster {
		return "in-cluster", nil
	}

	items := []list.Item{
		Item("in-cluster"),
		Item("atlas"),
	}

	l := list.New(items, ItemDelegate{}, DefaultWidth, ListHeight)
	l.Title = "Where will you be running MongoDB?"
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(false)
	l.Styles.Title = TitleStyle
	l.Styles.PaginationStyle = PaginationStyle
	l.Styles.HelpStyle = HelpStyle

	m := Model{List: l}

	model, err := tea.NewProgram(m).Run()
	if err != nil {
		log.Printf("Error running program: %v", err)
		return "", fmt.Errorf("failed to run the program: %w", err)
	}

	if strings.Contains(model.View(), "atlas") {
		return "atlas", nil
	}
	if strings.Contains(model.View(), "in-cluster") {
		return "in-cluster", nil
	}
	return "error", nil
}
