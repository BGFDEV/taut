// Package forms styles Huh forms from Taut's design tokens, so wizards look
// like the rest of the product rather than like a stock Huh theme.
package forms

import (
	"charm.land/bubbles/v2/help"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"

	"github.com/bgfdev/taut/internal/tui/theme"
)

// Theme returns a Huh theme derived from tokens. Huh passes in whether the
// background is dark; the glyph set comes from the base theme.
func Theme(base theme.Theme) huh.Theme {
	return huh.ThemeFunc(func(isDark bool) *huh.Styles {
		t := theme.New(theme.Options{Dark: isDark, Unicode: base.Unicode})
		c := t.Color
		s := huh.ThemeBase(isDark)

		border := lipgloss.ThickBorder()
		if !t.Unicode {
			border = lipgloss.Border{Left: "|"}
		}

		f := &s.Focused
		f.Base = lipgloss.NewStyle().PaddingLeft(t.Space.XS).BorderStyle(border).BorderLeft(true).BorderForeground(c.Brand)
		f.Card = f.Base
		f.Title = t.Style.Title.Foreground(c.Brand)
		f.NoteTitle = t.Style.Title.Foreground(c.Brand).MarginBottom(1)
		f.Description = t.Style.Muted
		f.ErrorIndicator = t.Style.Danger.SetString(" " + t.Glyph.Fail)
		f.ErrorMessage = t.Style.Danger
		f.SelectSelector = t.Style.Brand.SetString(t.Glyph.Pointer + " ")
		f.NextIndicator = t.Style.Muted.MarginLeft(1).SetString(t.Glyph.Arrow)
		f.PrevIndicator = t.Style.Muted.MarginRight(1)
		f.Option = t.Style.Text
		f.MultiSelectSelector = t.Style.Brand.SetString(t.Glyph.Pointer + " ")
		f.SelectedOption = t.Style.Success
		f.SelectedPrefix = t.Style.Success.SetString("[" + t.Glyph.OK + "] ")
		f.UnselectedOption = t.Style.Text
		f.UnselectedPrefix = t.Style.Faint.SetString("[ ] ")
		f.FocusedButton = lipgloss.NewStyle().Padding(0, t.Space.SM).MarginRight(1).Foreground(c.OnBrand).Background(c.Brand).Bold(true)
		f.BlurredButton = lipgloss.NewStyle().Padding(0, t.Space.SM).MarginRight(1).Foreground(c.Muted).Background(c.Surface)
		f.Next = f.FocusedButton
		f.TextInput.Cursor = lipgloss.NewStyle().Foreground(c.Accent)
		f.TextInput.Placeholder = t.Style.Faint
		f.TextInput.Prompt = t.Style.Brand
		f.TextInput.Text = t.Style.Text
		f.Directory = t.Style.Brand
		f.File = t.Style.Text

		s.Blurred = s.Focused
		s.Blurred.Base = s.Focused.Base.BorderStyle(lipgloss.HiddenBorder())
		s.Blurred.Card = s.Blurred.Base
		s.Blurred.Title = t.Style.Muted.Bold(true)
		s.Blurred.NoteTitle = t.Style.Muted.Bold(true).MarginBottom(1)
		s.Blurred.SelectSelector = lipgloss.NewStyle().SetString("  ")
		s.Blurred.MultiSelectSelector = lipgloss.NewStyle().SetString("  ")
		s.Blurred.NextIndicator = lipgloss.NewStyle()
		s.Blurred.PrevIndicator = lipgloss.NewStyle()
		s.Blurred.TextInput.Text = t.Style.Muted

		s.Group.Title = t.Style.Heading
		s.Group.Description = t.Style.Muted

		s.Help = help.Styles{
			Ellipsis:       t.Style.Faint,
			ShortKey:       t.Style.Key,
			ShortDesc:      t.Style.Muted,
			ShortSeparator: t.Style.Faint,
			FullKey:        t.Style.Key,
			FullDesc:       t.Style.Muted,
			FullSeparator:  t.Style.Faint,
		}
		return s
	})
}
