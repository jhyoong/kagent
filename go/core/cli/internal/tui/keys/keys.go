package keys

import "github.com/charmbracelet/bubbles/key"

type KeyMap struct {
	Quit     key.Binding
	Sessions key.Binding
	Agents   key.Binding
	Focus    key.Binding
	Details  key.Binding
	Fold     key.Binding
	Select   key.Binding
	Copy     key.Binding
	Layout   key.Binding
}

func DefaultKeyMap() KeyMap {
	return KeyMap{
		Quit: key.NewBinding(
			key.WithKeys("ctrl+c"),
			key.WithHelp("ctrl+c", "quit"),
		),
		Sessions: key.NewBinding(
			key.WithKeys("ctrl+s"),
			key.WithHelp("ctrl+s", "sessions"),
		),
		Agents: key.NewBinding(
			key.WithKeys("ctrl+a"),
			key.WithHelp("ctrl+a", "agents"),
		),
		Focus: key.NewBinding(
			key.WithKeys("tab"),
			key.WithHelp("tab", "focus"),
		),
		Details: key.NewBinding(
			key.WithKeys("ctrl+d"),
			key.WithHelp("ctrl+d", "details"),
		),
		Fold: key.NewBinding(
			key.WithKeys("ctrl+o"),
			key.WithHelp("ctrl+o", "expand tools"),
		),
		Select: key.NewBinding(
			key.WithKeys("ctrl+g"),
			key.WithHelp("ctrl+g", "select"),
		),
		Copy: key.NewBinding(
			key.WithKeys("ctrl+y"),
			key.WithHelp("ctrl+y", "copy answer"),
		),
		Layout: key.NewBinding(
			key.WithKeys("ctrl+l"),
			key.WithHelp("ctrl+l", "select layout"),
		),
	}
}

func (k KeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{k.ShortHelp()}
}

func (k KeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Quit, k.Sessions, k.Agents, k.Focus, k.Details, k.Fold, k.Select, k.Copy, k.Layout}
}
