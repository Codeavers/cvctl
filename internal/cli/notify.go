package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

var newNotifyEnv = func() (sys.FS, string) {
	return sys.NewOSFS(), "/etc/cvctl"
}

func newNotifyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "notify",
		Short: "Notification management and verification commands",
		Example: `  cvctl notify test
  cvctl notify test --message "custom alert"`,
	}
	cmd.AddCommand(newNotifyTestCmd())
	return cmd
}

func newNotifyTestCmd() *cobra.Command {
	var message string
	cmd := &cobra.Command{
		Use:   "test",
		Short: "Send a test notification",
		Example: `  cvctl notify test
  cvctl notify test --message "custom alert"
  cvctl notify test --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			text := message
			if text == "" {
				text = "cvctl notify test"
			}

			fsys, dir := newNotifyEnv()
			cfg, sec, err := hostcfg.Load(fsys, dir)
			if err != nil {
				return err
			}

			if cfg == nil || cfg.Notify == "" || cfg.Notify == "none" {
				return &output.Error{
					Code:    "E_NOTIFY_DISABLED",
					Message: "notifications are disabled",
					Hint:    "set CVCTL_NOTIFY=telegram in /etc/cvctl/host.env and run cvctl init",
				}
			}

			n, err := notify.FromHost(cfg, sec)
			if err != nil {
				return err
			}

			msg := notify.Message{
				Level: "info",
				Title: "test",
				Text:  text,
			}
			if err := n.Send(cmd.Context(), msg); err != nil {
				return err
			}

			printerFrom(cmd).Result(map[string]any{"sent": true}, func(w io.Writer) {
				fmt.Fprintln(w, "sent")
			})
			return nil
		},
	}
	cmd.Flags().StringVar(&message, "message", "", "message text to send")
	return cmd
}
