package cli

import "github.com/spf13/cobra"

// commands lists every top-level command in the order of a migration.
func commands(app *App) []*cobra.Command {
	return []*cobra.Command{
		newInitCmd(app),
		newAuthCmd(app),
		newPreflightCmd(app),
		newMapCmd(app),
		newMigrateCmd(app),
		newDashboardCmd(app),
		newFinalizeCmd(app),
		newAuditLogCmd(app),
		newStatusCmd(app),
		newDoctorCmd(app),
		newControllerCmd(app),
		newWorkerCmd(app),
		newSidecarCmd(app),
		newDevCmd(app),
	}
}
