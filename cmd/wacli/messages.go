package main

import "github.com/spf13/cobra"

func newMessagesCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "messages",
		Short: "List and search messages from the local DB",
	}
	cmd.AddCommand(newMessagesListCmd(flags))
	cmd.AddCommand(newMessagesSearchCmd(flags))
	cmd.AddCommand(newMessagesStarredCmd(flags))
	cmd.AddCommand(newMessagesShowCmd(flags))
	cmd.AddCommand(newMessagesContextCmd(flags))
	cmd.AddCommand(newMessagesExportCmd(flags))
	cmd.AddCommand(newMessagesDeleteCmd(flags))
	cmd.AddCommand(newMessagesPurgeCmd(flags))
	cmd.AddCommand(newMessagesRevokeCmd(flags))
	cmd.AddCommand(newMessagesEditCmd(flags))
	cmd.AddCommand(newMessagesForwardCmd(flags))
	cmd.AddCommand(newMessagesStarCmd(flags, true))
	cmd.AddCommand(newMessagesStarCmd(flags, false))
	cmd.AddCommand(newMessagesPinCmd(flags, true))
	cmd.AddCommand(newMessagesPinCmd(flags, false))
	cmd.AddCommand(newMessagesKeepCmd(flags, true))
	cmd.AddCommand(newMessagesKeepCmd(flags, false))
	cmd.AddCommand(newMessagesPinnedCmd(flags))
	return cmd
}
