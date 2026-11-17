package shell

import "charm.land/bubbles/v2/key"

// Keys is the global key map. Every screen inherits these bindings; screens
// add their own but cannot rebind these, which is what keeps navigation
// consistent across the whole product.
type Keys struct {
	Help key.Binding
	Quit key.Binding
	Back key.Binding

	// Navigation vocabulary shared by screens. The shell does not act on
	// these itself; they live here so every screen uses the same keys and
	// the same wording in the help bar.
	Up       key.Binding
	Down     key.Binding
	PageUp   key.Binding
	PageDown key.Binding
	Top      key.Binding
	Bottom   key.Binding
	Next     key.Binding // next section / pane
	Prev     key.Binding
	Select   key.Binding // drill in
	Filter   key.Binding
	Scroll   key.Binding // help-bar summary of Up/Down
}

// DefaultKeys returns the global key map with Unicode key hints.
func DefaultKeys() Keys { return NewKeys(true) }

// NewKeys returns the global key map. When unicode is false the help text
// for arrow keys is spelled out so the help bar stays pure ASCII.
func NewKeys(unicode bool) Keys {
	up, down := "↑/k", "↓/j"
	if !unicode {
		up, down = "up/k", "down/j"
	}
	return Keys{
		Help: key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit: key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Back: key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),

		Up:       key.NewBinding(key.WithKeys("up", "k"), key.WithHelp(up, "up")),
		Down:     key.NewBinding(key.WithKeys("down", "j"), key.WithHelp(down, "down")),
		PageUp:   key.NewBinding(key.WithKeys("pgup", "b"), key.WithHelp("pgup", "page up")),
		PageDown: key.NewBinding(key.WithKeys("pgdown", "space", "f"), key.WithHelp("pgdn", "page down")),
		Top:      key.NewBinding(key.WithKeys("home", "g"), key.WithHelp("g", "top")),
		Bottom:   key.NewBinding(key.WithKeys("end", "G"), key.WithHelp("G", "bottom")),
		Next:     key.NewBinding(key.WithKeys("tab", "right", "l"), key.WithHelp("tab", "next section")),
		Prev:     key.NewBinding(key.WithKeys("shift+tab", "left", "h"), key.WithHelp("shift+tab", "previous section")),
		Select:   key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open")),
		Filter:   key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
		Scroll:   key.NewBinding(key.WithKeys("up", "down"), key.WithHelp(scrollHint(unicode), "scroll")),
	}
}

func scrollHint(unicode bool) string {
	if unicode {
		return "↑/↓"
	}
	return "up/down"
}
