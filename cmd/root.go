package cmd

import (
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

func Execute() {
	lifetimeCtx := ctrl.SetupSignalHandler()

	RootCmd.ExecuteContext(lifetimeCtx)
}
