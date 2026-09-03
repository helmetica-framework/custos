package cmd

import (
	"os"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
)

var RootCmd = &cobra.Command{
	Use:   "custos",
	Short: "custos manages arcana.",
	Long:  "custos manages arcana.",
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		cmd.SilenceUsage = true
	},
}

// Execute runs the root command and turns a command error into a non-zero
// exit code.
func Execute() {
	lifetimeCtx := ctrl.SetupSignalHandler()

	if err := RootCmd.ExecuteContext(lifetimeCtx); err != nil {
		os.Exit(1)
	}
}
