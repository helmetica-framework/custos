package cmd

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// subprocessEnv marks the re-executed test binary. Execute ends in os.Exit, so
// observing what it does means running it in a child process, and without the
// marker that child would run the parent half of the test and fork forever.
const subprocessEnv = "CUSTOS_EXECUTE_SUBPROCESS"

// runInSubprocess re-runs one test in a child process and reports how that
// child exited.
func runInSubprocess(t *testing.T, name string) error {
	t.Helper()

	child := exec.Command(os.Args[0], "-test.run="+name)
	child.Env = append(os.Environ(), subprocessEnv+"=1")

	return child.Run()
}

// inSubprocess reports whether this is the child, and registers cmd as the one
// thing the root command will run.
func inSubprocess(cmd *cobra.Command) bool {
	if os.Getenv(subprocessEnv) != "1" {
		return false
	}

	RootCmd.AddCommand(cmd)
	RootCmd.SetArgs([]string{cmd.Use})

	return true
}

// The gather Job's outcome is read off this exit code and nothing else. A
// command that prints an error and exits 0 is a completed pod, so the Job
// succeeds on its first attempt, backoffLimit never applies, and a gather that
// failed is reported as one that mysteriously produced no values.
func TestExecute_AFailingCommandExitsNonZero(t *testing.T) {
	if inSubprocess(&cobra.Command{
		Use:  "boom",
		RunE: func(*cobra.Command, []string) error { return errors.New("boom") },
	}) {
		Execute()

		return
	}

	err := runInSubprocess(t, "TestExecute_AFailingCommandExitsNonZero")

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "a command whose RunE returns an error must not exit 0")
	assert.Equal(t, 1, exitErr.ExitCode())
}

// The other half of the contract, which an Execute that always exited non-zero
// would fail while still passing the test above.
func TestExecute_ASucceedingCommandExitsZero(t *testing.T) {
	if inSubprocess(&cobra.Command{
		Use:  "fine",
		RunE: func(*cobra.Command, []string) error { return nil },
	}) {
		Execute()

		return
	}

	assert.NoError(t, runInSubprocess(t, "TestExecute_ASucceedingCommandExitsZero"))
}
