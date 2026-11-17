package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/local"
	"github.com/bgfdev/taut/internal/tui/checklist"
)

// Local mode. A config file with `local.enabled: true` has no controller to
// reach: the command starts one in its own process, with everything it
// needs, and stops it when the command ends. The commands themselves are
// the same. They ask for a controller client and get one.

// control returns a client for the control plane of the config file's
// migration and a function that releases it.
//
// For a cluster that is a client for controller.url. In local mode it
// starts the local stack, or attaches to the one another taut process is
// running from the same state directory. work says whether the command
// imports or finalizes: such a command needs the stack's worker and must
// be the only process using the state directory, and is refused otherwise.
// A command that only reads leaves work false.
//
// A command that asks twice gets the same stack.
func (a *App) control(ctx context.Context, m *config.Config, work bool) (*controller.Client, func(), error) {
	if !m.Local.Enabled {
		return a.controllerClient(m), func() {}, nil
	}
	if a.stack != nil {
		return a.stack.Client(), func() {}, nil
	}
	s, err := local.Open(ctx, local.Options{Config: m, Getenv: a.Getenv, Tell: a.IO.Status, Work: work})
	if err != nil {
		return nil, nil, err
	}
	a.stack = s
	return s.Client(), func() {
		if s.Owner {
			a.IO.Status("Stopping the local ledger…")
		}
		s.Close()
		a.stack = nil
	}, nil
}

// closeStack stops the local stack a command opened without keeping the
// release function control gave it.
func (a *App) closeStack() {
	if a.stack != nil {
		a.stack.Close()
		a.stack = nil
	}
}

// isLocal reports whether the command is running a local stack of its own:
// the work stops when the command does.
func (a *App) isLocal() bool { return a.stack != nil && a.stack.Owner }

// errNoDetach refuses --detach in local mode.
func errNoDetach(command string) error {
	return fail.New("local.no_detach", "--detach does not apply in local mode",
		"With local.enabled the migration runs inside this command; there is nothing in the background to leave it to.",
		"Run `taut "+command+"` without --detach and leave it running. Stopping it is safe: run it again to resume.").WithExit(fail.ExitUsage)
}

// runConfig returns the config file as it is handed to the control plane
// when a run is created. A cluster gets the file as written. In local mode
// secret file references are made absolute first: the token broker reads
// only absolute paths, and the run's config is all it is given.
func (a *App) runConfig(m *config.Config) ([]byte, error) {
	if !m.Local.Enabled {
		raw, err := os.ReadFile(a.ConfigPath)
		if err != nil {
			return nil, fail.Wrap(err, "config.unreadable", "The config file could not be read", "", "Check the file's permissions.")
		}
		return raw, nil
	}
	return local.Absolute(m).Marshal()
}

// localChecks are what `taut doctor` checks for a config file in local mode,
// in place of the controller.
func localChecks(loaded func() *config.Config) []checklist.Check {
	return []checklist.Check{
		{Group: groupStack, Label: "State directory", Run: func(context.Context) checklist.Result {
			m := loaded()
			dir := m.StateDir()
			st, err := os.Stat(dir)
			switch {
			case os.IsNotExist(err):
				if parent, perr := os.Stat(filepath.Dir(dir)); perr != nil || !parent.IsDir() {
					return checklist.Fail("local.state_dir", filepath.Dir(dir)+" does not exist",
						"Create it, or set local.stateDir in the config file.")
				}
				return checklist.OK(dir + " will be created by `taut migrate`")
			case err != nil:
				return checklist.Fail("local.state_dir", err.Error(), "Check the permissions of "+dir+".")
			case !st.IsDir():
				return checklist.Fail("local.state_dir", dir+" is not a directory", "Set local.stateDir to a directory.")
			case st.Mode().Perm()&0o077 != 0:
				return checklist.Warn(fmt.Sprintf("%s is readable by other users (mode %04o)", dir, st.Mode().Perm()),
					"It holds the migration's ledger, staged data and their key. Run `chmod 700 "+dir+"`.")
			}
			if rt, ok := local.ReadRuntime(dir); ok {
				return checklist.Info(fmt.Sprintf("%s, in use by taut process %d", dir, rt.PID))
			}
			return checklist.OK(dir)
		}},
		{Group: groupStack, Label: "PostgreSQL", Run: func(ctx context.Context) checklist.Result {
			m := loaded()
			pg, err := local.FindPostgres(ctx, local.Search{
				Binaries: m.ResolvePath(m.Local.Postgres.Binaries), Download: m.Local.Postgres.Download,
				Major: local.DataMajor(m.StateDir()),
			})
			if err != nil {
				fe := fail.From(err)
				return checklist.Fail(fe.Code, fe.What+": "+fe.Why, fe.Next)
			}
			where := map[string]string{
				"config": "from local.postgres.binaries", "path": "found on PATH",
				"system": "found installed", "cache": "downloaded earlier",
			}[pg.Origin]
			if pg.NeedsDownload() {
				return checklist.Info("not installed; `taut migrate` will download it from repo1.maven.org into " + filepath.Dir(pg.Dir) + " (local.postgres.download is set)")
			}
			return checklist.OK(fmt.Sprintf("version %d at %s, %s", pg.Major, pg.Dir, where))
		}},
		{Group: groupStack, Label: "Isolation", Run: func(context.Context) checklist.Result {
			return checklist.Info("local mode: the worker, the token broker and the egress proxy share this process and this user account")
		}},
	}
}
