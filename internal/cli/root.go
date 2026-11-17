// Package cli defines the taut command tree.
package cli

import (
	"context"
	"errors"
	"image/color"
	"io"
	"os"
	"slices"
	"strings"
	"syscall"

	"charm.land/fang/v2"
	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/bgfdev/taut/internal/local"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/tui/theme"
	"github.com/bgfdev/taut/internal/ui"
	"github.com/bgfdev/taut/internal/version"
)

// App is the state shared by every command in one invocation.
type App struct {
	IO *ui.IO

	// Global flags.
	ConfigPath string
	JSON       bool
	Yes        bool
	NoColor    bool
	Verbose    bool

	in       io.Reader
	out, err io.Writer
	env      []string

	// stack is the local stack this command is using, in local mode.
	stack *local.Stack
}

// Getenv looks up a variable in the invocation's environment.
func (a *App) Getenv(key string) string {
	for i := len(a.env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(a.env[i], key+"="); ok {
			return v
		}
	}
	return ""
}

func (a *App) buildIO() {
	a.IO = ui.New(a.in, a.out, a.err, a.env, ui.Options{JSON: a.JSON, Yes: a.Yes, NoColor: a.NoColor})
}

const (
	groupPlan    = "plan"
	groupRun     = "run"
	groupInspect = "inspect"
	groupMore    = "more"
)

// NewRoot builds the command tree.
func NewRoot(app *App) *cobra.Command {
	root := &cobra.Command{
		Use:   "taut",
		Short: "Migrate Slack workspaces to Microsoft Teams",
		Long: `Taut migrates Slack workspaces — channels, threads, files and people —
into Microsoft Teams, preserving authors and original timestamps.

A migration is described by a config file (taut.yaml). The usual order:

  taut init        write a config file
  taut auth        verify credentials and permissions
  taut preflight   see exactly what will and won't migrate
  taut migrate     run the migration
  taut finalize    open the channels and add their members`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			app.buildIO()
		},
	}
	root.SetIn(app.in)
	root.SetOut(app.out)
	root.SetErr(app.err)

	pf := root.PersistentFlags()
	pf.StringVarP(&app.ConfigPath, "config", "c", "taut.yaml", "path to the config file")
	pf.BoolVar(&app.JSON, "json", false, "print one JSON document instead of styled output")
	pf.BoolVarP(&app.Yes, "yes", "y", false, "never prompt; accept defaults")
	pf.BoolVar(&app.NoColor, "no-color", false, "disable color (NO_COLOR is also honored)")
	pf.BoolVarP(&app.Verbose, "verbose", "v", false, "include diagnostic detail")

	root.AddGroup(
		&cobra.Group{ID: groupPlan, Title: "Plan"},
		&cobra.Group{ID: groupRun, Title: "Run"},
		&cobra.Group{ID: groupInspect, Title: "Inspect"},
		&cobra.Group{ID: groupMore, Title: "More"},
	)
	root.SetHelpCommandGroupID(groupMore)
	root.SetCompletionCommandGroupID(groupMore)
	for _, c := range commands(app) {
		root.AddCommand(c)
	}
	return root
}

// Main runs taut and returns the process exit code.
func Main(ctx context.Context, args []string, in io.Reader, out, errw io.Writer, env []string) int {
	app := &App{in: in, out: out, err: errw, env: env}
	// Flags are parsed by Cobra, but errors raised before that (an unknown
	// flag, say) must still honor --json and --no-color, so peek at them.
	app.JSON = slices.Contains(args, "--json")
	app.NoColor = slices.Contains(args, "--no-color")
	app.buildIO()

	root := NewRoot(app)
	root.SetArgs(args)

	var runErr error
	err := fang.Execute(ctx, root,
		fang.WithVersion(version.Version),
		fang.WithCommit(version.Commit),
		fang.WithColorSchemeFunc(fangScheme(app.IO.Theme.Unicode)),
		fang.WithNotifySignal(os.Interrupt, syscall.SIGTERM),
		fang.WithErrorHandler(func(_ io.Writer, _ fang.Styles, err error) {
			runErr = normalize(err)
			app.IO.Error(runErr)
		}),
	)
	// Whatever a command left running goes down with it.
	app.closeStack()
	if err == nil {
		return fail.ExitOK
	}
	if runErr == nil {
		runErr = normalize(err)
	}
	return fail.ExitCode(runErr)
}

// normalize turns Cobra's own errors into structured usage errors.
func normalize(err error) error {
	var fe *fail.Error
	if errors.As(err, &fe) {
		return err
	}
	msg := err.Error()
	for _, p := range []string{"unknown command", "unknown flag", "unknown shorthand", "flag needs an argument", "invalid argument", "accepts ", "requires ", "required flag"} {
		if strings.HasPrefix(msg, p) {
			return fail.Wrap(err, "usage", "That command line isn't valid", msg,
				"Run `taut --help` to see commands and flags.").WithExit(fail.ExitUsage)
		}
	}
	return fail.From(err)
}

// fangScheme maps the design tokens onto Fang's help styling so `--help`
// matches the rest of the product, in light and dark terminals alike.
func fangScheme(unicode bool) fang.ColorSchemeFunc {
	light := theme.New(theme.Options{Dark: false, Unicode: unicode}).Color
	dark := theme.New(theme.Options{Dark: true, Unicode: unicode}).Color
	return func(pick lipgloss.LightDarkFunc) fang.ColorScheme {
		return fang.ColorScheme{
			Base:           pick(light.Ink, dark.Ink),
			Title:          pick(light.Brand, dark.Brand),
			Description:    pick(light.Muted, dark.Muted),
			Codeblock:      pick(light.Surface, dark.Surface),
			Program:        pick(light.Brand, dark.Brand),
			DimmedArgument: pick(light.Muted, dark.Muted),
			Comment:        pick(light.Muted, dark.Muted),
			Flag:           pick(light.Accent, dark.Accent),
			FlagDefault:    pick(light.Muted, dark.Muted),
			Command:        pick(light.Info, dark.Info),
			QuotedString:   pick(light.Success, dark.Success),
			Argument:       pick(light.Ink, dark.Ink),
			Help:           pick(light.Muted, dark.Muted),
			Dash:           pick(light.Faint, dark.Faint),
			ErrorHeader:    [2]color.Color{pick(light.OnBrand, dark.OnBrand), pick(light.Danger, dark.Danger)},
			ErrorDetails:   pick(light.Danger, dark.Danger),
		}
	}
}
