package cli

import (
	"fmt"
	"time"

	"github.com/coder/coder/cli/clibase"
	"github.com/coder/coder/cli/cliui"
	"github.com/coder/coder/codersdk"
)

func start() *clibase.Cmd {
	cmd := &clibase.Cmd{
		Annotations: workspaceCommand,
		Use:         "start <workspace>",
		Short:       "Start a workspace",
		Middleware:  clibase.RequireNArgs(1),
		Handler: func(inv *clibase.Invokation) error {
			client, err := useClient(cmd)
			if err != nil {
				return err
			}
			workspace, err := namedWorkspace(cmd, client, inv.Args[0])
			if err != nil {
				return err
			}
			build, err := client.CreateWorkspaceBuild(inv.Context(), workspace.ID, codersdk.CreateWorkspaceBuildRequest{
				Transition: codersdk.WorkspaceTransitionStart,
			})
			if err != nil {
				return err
			}

			err = cliui.WorkspaceBuild(inv.Context(), inv.Stdout, client, build.ID)
			if err != nil {
				return err
			}

			_, _ = fmt.Fprintf(inv.Stdout, "\nThe %s workspace has been started at %s!\n", cliui.Styles.Keyword.Render(workspace.Name), cliui.Styles.DateTimeStamp.Render(time.Now().Format(time.Stamp)))
			return nil
		},
	}
	cliui.AllowSkipPrompt(inv)
	return cmd
}
