package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow/types"
)

// favoritesListRef always names the favorites list, whatever the phone's
// language calls it.
const favoritesListRef = "favorites"

func newChatsListsCmd(flags *rootFlags) *cobra.Command {
	var includeDeleted bool
	cmd := &cobra.Command{
		Use:   "lists",
		Short: "Show WhatsApp lists (chat filters) and manage custom lists",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newApp(ctx, flags, false, false)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			lists, err := loadChatListViews(ctx, a, includeDeleted)
			if err != nil {
				return err
			}
			if flags.asJSON {
				return out.WriteJSON(os.Stdout, lists)
			}
			fullOutput := fullTableOutput(flags.fullOutput)
			w := newTableWriter(os.Stdout)
			fmt.Fprintln(w, "ID\tNAME\tTYPE\tCHATS")
			for _, l := range lists {
				count := fmt.Sprint(l.Count)
				if l.Computed {
					count = "(computed)"
				}
				name := l.Name
				if l.Deleted {
					name += " (deleted)"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", l.ID, tableCell(name, 32, fullOutput), l.Type, count)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&includeDeleted, "include-deleted", false, "also show lists deleted on WhatsApp")
	cmd.AddCommand(newChatsListsCreateCmd(flags))
	cmd.AddCommand(newChatsListsRenameCmd(flags))
	cmd.AddCommand(newChatsListsDeleteCmd(flags))
	cmd.AddCommand(newChatsListsMemberCmd(flags, true))
	cmd.AddCommand(newChatsListsMemberCmd(flags, false))
	return cmd
}

func newChatsListsCreateCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "create NAME",
		Short: "Create a custom list",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(args[0]) == "" {
				return fmt.Errorf("list name is required")
			}
			return runChatsMessagesCommand(flags, sendDelegateRequest{Kind: chatListCreateKind, Name: args[0]})
		},
	}
}

func newChatsListsRenameCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "rename LIST NEW_NAME",
		Short: "Rename a custom list (LIST is its ID or name)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(args[1]) == "" {
				return fmt.Errorf("new list name is required")
			}
			return runChatsMessagesCommand(flags, sendDelegateRequest{Kind: chatListRenameKind, List: args[0], Name: args[1]})
		},
	}
}

func newChatsListsDeleteCmd(flags *rootFlags) *cobra.Command {
	var confirm bool
	cmd := &cobra.Command{
		Use:   "delete LIST",
		Short: "Delete a custom list (its chats are not changed)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := flags.requireWritable(); err != nil {
				return err
			}
			if err := confirmChatAction(flags, confirm, fmt.Sprintf("Delete list %s on WhatsApp for all your devices?", sanitize(args[0]))); err != nil {
				return err
			}
			return runChatsMessagesCommand(flags, sendDelegateRequest{Kind: chatListDeleteKind, List: args[0]})
		},
	}
	cmd.Flags().BoolVar(&confirm, "confirm", false, "skip the confirmation prompt")
	return cmd
}

func newChatsListsMemberCmd(flags *rootFlags, add bool) *cobra.Command {
	use, short, kind := "add", "Add a chat to a custom list", chatListAddKind
	if !add {
		use, short, kind = "remove", "Remove a chat from a custom list", chatListRemoveKind
	}
	var list string
	opts := chatActionOptions{}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(list) == "" {
				return fmt.Errorf("--list is required")
			}
			if err := requireChatFlag(opts.chat); err != nil {
				return err
			}
			return runChatsMessagesCommand(flags, sendDelegateRequest{Kind: kind, List: list, To: opts.chat, Pick: opts.pick})
		},
	}
	cmd.Flags().StringVar(&list, "list", "", "list ID or name")
	addChatActionFlags(cmd, &opts)
	return cmd
}

func runChatListDefinition(ctx context.Context, a chatsMessagesApp, req sendDelegateRequest) (sendDelegateResponse, error) {
	if req.Kind != chatListCreateKind && strings.TrimSpace(req.List) == "" {
		return sendDelegateResponse{}, fmt.Errorf("a list ID or name is required")
	}
	var l store.ChatList
	var err error
	switch req.Kind {
	case chatListCreateKind:
		l, err = a.CreateChatList(ctx, req.Name)
	case chatListRenameKind:
		l, err = a.RenameChatList(ctx, req.List, req.Name)
	default:
		l, err = a.DeleteChatList(ctx, req.List)
	}
	if err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true, Action: actionName(req.Kind), ListID: l.ID, Name: l.Name}, nil
}

func runChatListMembership(ctx context.Context, a chatsMessagesApp, req sendDelegateRequest, ropts recipientOptions) (sendDelegateResponse, error) {
	if strings.TrimSpace(req.List) == "" {
		return sendDelegateResponse{}, fmt.Errorf("--list is required")
	}
	if strings.EqualFold(strings.TrimSpace(req.List), favoritesListRef) {
		return sendDelegateResponse{}, fmt.Errorf("use `wacli chats favorite` and `wacli chats unfavorite` for favorites")
	}
	jid, err := resolveRecipient(a, req.To, ropts)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	l, err := a.SetChatListMember(ctx, req.List, jid, req.Kind == chatListAddKind)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true, Action: actionName(req.Kind), Chat: jid.String(), ListID: l.ID, Name: l.Name}, nil
}

// chatListView is one list as `chats lists` prints it.
type chatListView struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Color    int32    `json:"color"`
	Order    *int32   `json:"order,omitempty"`
	Deleted  bool     `json:"deleted,omitempty"`
	Computed bool     `json:"computed,omitempty"`
	Count    int      `json:"count"`
	Members  []string `json:"members"`
}

func loadChatListViews(ctx context.Context, a *app.App, includeDeleted bool) ([]chatListView, error) {
	lists, err := a.DB().ListChatLists(includeDeleted)
	if err != nil {
		return nil, err
	}
	favorites, err := favoriteChatJIDs(a.DB())
	if err != nil {
		return nil, err
	}
	resolver := localChatResolver(a)
	views := make([]chatListView, 0, len(lists)+1)
	sawFavorites := false
	for _, l := range lists {
		view := chatListView{
			ID:       l.ID,
			Name:     strings.TrimSpace(strings.Trim(l.Name, "‎‏")),
			Type:     app.ChatListTypeName(l.ListType),
			Color:    l.Color,
			Order:    l.OrderIndex,
			Deleted:  l.Deleted,
			Computed: app.IsComputedListType(l.ListType),
			Members:  []string{},
		}
		switch {
		case app.IsFavoritesListType(l.ListType):
			sawFavorites = true
			view.Members = displayChatJIDs(ctx, resolver, favorites)
		case !view.Computed:
			members, err := listMemberChatJIDs(a.DB(), l.ID)
			if err != nil {
				return nil, err
			}
			view.Members = displayChatJIDs(ctx, resolver, members)
		}
		view.Count = len(view.Members)
		views = append(views, view)
	}
	if !sawFavorites && len(favorites) > 0 {
		members := displayChatJIDs(ctx, resolver, favorites)
		views = append(views, chatListView{Name: "Favorites", Type: favoritesListRef, Members: members, Count: len(members)})
	}
	return views, nil
}

// chatListFilterJIDs is every stored chat JID of a list's members, under both
// their phone and LID JIDs, for `chats list --list`.
func chatListFilterJIDs(ctx context.Context, a *app.App, ref string) ([]string, error) {
	var members []string
	if strings.EqualFold(strings.TrimSpace(ref), favoritesListRef) {
		favorites, err := favoriteChatJIDs(a.DB())
		if err != nil {
			return nil, err
		}
		members = favorites
	} else {
		lists, err := a.DB().ListChatLists(false)
		if err != nil {
			return nil, err
		}
		l, ok := store.FindChatList(lists, ref)
		if !ok {
			return nil, fmt.Errorf("no list matches %q (see `wacli chats lists`)", ref)
		}
		switch {
		case app.IsFavoritesListType(l.ListType):
			members, err = favoriteChatJIDs(a.DB())
		case app.IsComputedListType(l.ListType):
			return nil, fmt.Errorf("list %q is computed by WhatsApp on the phone; use the chats list filters (for example --unread) instead", l.Name)
		default:
			members, err = listMemberChatJIDs(a.DB(), l.ID)
		}
		if err != nil {
			return nil, err
		}
	}
	resolver := localChatResolver(a)
	out := make([]string, 0, len(members)*2)
	for _, raw := range members {
		out = append(out, raw)
		jid, err := types.ParseJID(raw)
		if err != nil || resolver == nil {
			continue
		}
		switch jid.Server {
		case types.HiddenUserServer:
			out = append(out, resolver.ResolveLIDToPN(ctx, jid).String())
		case types.DefaultUserServer:
			out = append(out, resolver.ResolvePNToLID(ctx, jid).String())
		}
	}
	return out, nil
}

func listMemberChatJIDs(db *store.DB, listID string) ([]string, error) {
	members, err := db.ChatListMembers(listID, true)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(members))
	for _, m := range members {
		out = append(out, m.ChatJID)
	}
	return out, nil
}

func favoriteChatJIDs(db *store.DB) ([]string, error) {
	favorites, err := db.ListFavorites()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(favorites))
	for _, f := range favorites {
		out = append(out, f.ChatJID)
	}
	return out, nil
}

// displayChatJIDs shows members under their phone JID when the session knows
// it, once each, keeping the list's order.
func displayChatJIDs(ctx context.Context, resolver app.LocalResolver, jids []string) []string {
	out := make([]string, 0, len(jids))
	for _, raw := range jids {
		shown := raw
		if jid, err := types.ParseJID(raw); err == nil && jid.Server == types.HiddenUserServer && resolver != nil {
			if pn := resolver.ResolveLIDToPN(ctx, jid); pn.Server == types.DefaultUserServer {
				shown = pn.ToNonAD().String()
			}
		}
		if !slices.Contains(out, shown) {
			out = append(out, shown)
		}
	}
	return out
}

func localChatResolver(a *app.App) app.LocalResolver {
	if _, err := os.Stat(filepath.Join(a.StoreDir(), "session.db")); err != nil {
		return nil
	}
	resolver, err := a.LocalResolver()
	if err != nil {
		return nil
	}
	return resolver
}
