package main

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix/util/variationselector"

	"github.com/bwmarrin/discordgo"

	log "maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridge"
	"maunium.net/go/mautrix/bridge/bridgeconfig"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-discord/database"
)

type portalDiscordMessage struct {
	msg  interface{}
	user *User

	thread *Thread
}

type portalMatrixMessage struct {
	evt  *event.Event
	user *User
}

type Portal struct {
	*database.Portal

	Parent *Portal
	Guild  *Guild

	bridge *DiscordBridge
	log    log.Logger

	roomCreateLock sync.Mutex
	encryptLock    sync.Mutex

	discordMessages chan portalDiscordMessage
	matrixMessages  chan portalMatrixMessage
}

func (portal *Portal) IsEncrypted() bool {
	return portal.Encrypted
}

func (portal *Portal) MarkEncrypted() {
	portal.Encrypted = true
	portal.Update()
}

func (portal *Portal) ReceiveMatrixEvent(user bridge.User, evt *event.Event) {
	if user.GetPermissionLevel() >= bridgeconfig.PermissionLevelUser /*|| portal.HasRelaybot()*/ {
		portal.matrixMessages <- portalMatrixMessage{user: user.(*User), evt: evt}
	}
}

var _ bridge.Portal = (*Portal)(nil)

var (
	portalCreationDummyEvent = event.Type{Type: "fi.mau.dummy.portal_created", Class: event.MessageEventType}
)

func (br *DiscordBridge) loadPortal(dbPortal *database.Portal, key *database.PortalKey, chanType discordgo.ChannelType) *Portal {
	if dbPortal == nil {
		if key == nil || chanType < 0 {
			return nil
		}

		dbPortal = br.DB.Portal.New()
		dbPortal.Key = *key
		dbPortal.Type = chanType
		dbPortal.Insert()
	}

	portal := br.NewPortal(dbPortal)

	br.portalsByID[portal.Key] = portal
	if portal.MXID != "" {
		br.portalsByMXID[portal.MXID] = portal
	}

	if portal.GuildID != "" {
		portal.Guild = portal.bridge.GetGuildByID(portal.GuildID, true)
	}
	if portal.ParentID != "" {
		parentKey := database.NewPortalKey(portal.ParentID, "")
		var ok bool
		portal.Parent, ok = br.portalsByID[parentKey]
		if !ok {
			portal.Parent = br.loadPortal(br.DB.Portal.GetByID(parentKey), nil, -1)
		}
	}

	return portal
}

func (br *DiscordBridge) GetPortalByMXID(mxid id.RoomID) *Portal {
	br.portalsLock.Lock()
	defer br.portalsLock.Unlock()

	portal, ok := br.portalsByMXID[mxid]
	if !ok {
		return br.loadPortal(br.DB.Portal.GetByMXID(mxid), nil, -1)
	}

	return portal
}

func (user *User) GetPortalByMeta(meta *discordgo.Channel) *Portal {
	return user.GetPortalByID(meta.ID, meta.Type)
}

func (user *User) GetExistingPortalByID(id string) *Portal {
	return user.bridge.GetExistingPortalByID(database.NewPortalKey(id, user.DiscordID))
}

func (user *User) GetPortalByID(id string, chanType discordgo.ChannelType) *Portal {
	return user.bridge.GetPortalByID(database.NewPortalKey(id, user.DiscordID), chanType)
}

func (br *DiscordBridge) GetExistingPortalByID(key database.PortalKey) *Portal {
	br.portalsLock.Lock()
	defer br.portalsLock.Unlock()
	portal, ok := br.portalsByID[key]
	if !ok {
		portal, ok = br.portalsByID[database.NewPortalKey(key.ChannelID, "")]
		if !ok {
			return br.loadPortal(br.DB.Portal.GetByID(key), nil, -1)
		}
	}

	return portal
}

func (br *DiscordBridge) GetPortalByID(key database.PortalKey, chanType discordgo.ChannelType) *Portal {
	br.portalsLock.Lock()
	defer br.portalsLock.Unlock()
	if chanType != discordgo.ChannelTypeDM {
		key.Receiver = ""
	}

	portal, ok := br.portalsByID[key]
	if !ok {
		return br.loadPortal(br.DB.Portal.GetByID(key), &key, chanType)
	}

	return portal
}

func (br *DiscordBridge) GetAllPortals() []*Portal {
	return br.dbPortalsToPortals(br.DB.Portal.GetAll())
}

func (br *DiscordBridge) GetDMPortalsWith(otherUserID string) []*Portal {
	return br.dbPortalsToPortals(br.DB.Portal.FindPrivateChatsWith(otherUserID))
}

func (br *DiscordBridge) dbPortalsToPortals(dbPortals []*database.Portal) []*Portal {
	br.portalsLock.Lock()
	defer br.portalsLock.Unlock()

	output := make([]*Portal, len(dbPortals))
	for index, dbPortal := range dbPortals {
		if dbPortal == nil {
			continue
		}

		portal, ok := br.portalsByID[dbPortal.Key]
		if !ok {
			portal = br.loadPortal(dbPortal, nil, -1)
		}

		output[index] = portal
	}

	return output
}

func (br *DiscordBridge) NewPortal(dbPortal *database.Portal) *Portal {
	portal := &Portal{
		Portal: dbPortal,
		bridge: br,
		log:    br.Log.Sub(fmt.Sprintf("Portal/%s", dbPortal.Key)),

		discordMessages: make(chan portalDiscordMessage, br.Config.Bridge.PortalMessageBuffer),
		matrixMessages:  make(chan portalMatrixMessage, br.Config.Bridge.PortalMessageBuffer),
	}

	go portal.messageLoop()

	return portal
}

func (portal *Portal) messageLoop() {
	for {
		select {
		case msg := <-portal.matrixMessages:
			portal.handleMatrixMessages(msg)
		case msg := <-portal.discordMessages:
			portal.handleDiscordMessages(msg)
		}
	}
}

func (portal *Portal) IsPrivateChat() bool {
	return portal.Type == discordgo.ChannelTypeDM
}

func (portal *Portal) MainIntent() *appservice.IntentAPI {
	if portal.IsPrivateChat() && portal.OtherUserID != "" {
		return portal.bridge.GetPuppetByID(portal.OtherUserID).DefaultIntent()
	}

	return portal.bridge.Bot
}

func (portal *Portal) getBridgeInfo() (string, event.BridgeEventContent) {
	bridgeInfo := event.BridgeEventContent{
		BridgeBot: portal.bridge.Bot.UserID,
		Creator:   portal.MainIntent().UserID,
		Protocol: event.BridgeInfoSection{
			ID:          "discord",
			DisplayName: "Discord",
			AvatarURL:   portal.bridge.Config.AppService.Bot.ParsedAvatar.CUString(),
			ExternalURL: "https://discord.com/",
		},
		Channel: event.BridgeInfoSection{
			ID:          portal.Key.ChannelID,
			DisplayName: portal.Name,
		},
	}
	var bridgeInfoStateKey string
	if portal.GuildID == "" {
		bridgeInfoStateKey = fmt.Sprintf("fi.mau.discord://discord/dm/%s", portal.Key.ChannelID)
	} else {
		bridgeInfo.Network = &event.BridgeInfoSection{
			ID: portal.GuildID,
		}
		if portal.Guild != nil {
			bridgeInfo.Network.DisplayName = portal.Guild.Name
			bridgeInfo.Network.AvatarURL = portal.Guild.AvatarURL.CUString()
			// TODO is it possible to find the URL?
		}
		bridgeInfoStateKey = fmt.Sprintf("fi.mau.discord://discord/%s/%s", portal.GuildID, portal.Key.ChannelID)
	}
	return bridgeInfoStateKey, bridgeInfo
}

func (portal *Portal) UpdateBridgeInfo() {
	if len(portal.MXID) == 0 {
		portal.log.Debugln("Not updating bridge info: no Matrix room created")
		return
	}
	portal.log.Debugln("Updating bridge info...")
	stateKey, content := portal.getBridgeInfo()
	_, err := portal.MainIntent().SendStateEvent(portal.MXID, event.StateBridge, stateKey, content)
	if err != nil {
		portal.log.Warnln("Failed to update m.bridge:", err)
	}
	// TODO remove this once https://github.com/matrix-org/matrix-doc/pull/2346 is in spec
	_, err = portal.MainIntent().SendStateEvent(portal.MXID, event.StateHalfShotBridge, stateKey, content)
	if err != nil {
		portal.log.Warnln("Failed to update uk.half-shot.bridge:", err)
	}
}

func (portal *Portal) CreateMatrixRoom(user *User, channel *discordgo.Channel) error {
	portal.roomCreateLock.Lock()
	defer portal.roomCreateLock.Unlock()
	if portal.MXID != "" {
		return nil
	}
	portal.log.Infoln("Creating Matrix room for channel")

	channel = portal.UpdateInfo(user, channel)

	intent := portal.MainIntent()
	if err := intent.EnsureRegistered(); err != nil {
		return err
	}

	bridgeInfoStateKey, bridgeInfo := portal.getBridgeInfo()
	initialState := []*event.Event{{
		Type:     event.StateBridge,
		Content:  event.Content{Parsed: bridgeInfo},
		StateKey: &bridgeInfoStateKey,
	}, {
		// TODO remove this once https://github.com/matrix-org/matrix-doc/pull/2346 is in spec
		Type:     event.StateHalfShotBridge,
		Content:  event.Content{Parsed: bridgeInfo},
		StateKey: &bridgeInfoStateKey,
	}}

	if !portal.AvatarURL.IsEmpty() {
		initialState = append(initialState, &event.Event{
			Type: event.StateRoomAvatar,
			Content: event.Content{Parsed: &event.RoomAvatarEventContent{
				URL: portal.AvatarURL,
			}},
		})
	}

	creationContent := make(map[string]interface{})
	if portal.Type == discordgo.ChannelTypeGuildCategory {
		creationContent["type"] = event.RoomTypeSpace
	}
	if !portal.bridge.Config.Bridge.FederateRooms {
		creationContent["m.federate"] = false
	}
	spaceID := portal.ExpectedSpaceID()
	if spaceID != "" {
		spaceIDStr := spaceID.String()
		initialState = append(initialState, &event.Event{
			Type:     event.StateSpaceParent,
			StateKey: &spaceIDStr,
			Content: event.Content{Parsed: &event.SpaceParentEventContent{
				Via:       []string{portal.bridge.AS.HomeserverDomain},
				Canonical: true,
			}},
		})
	}
	if portal.Guild != nil && portal.Guild.MXID != "" {
		// TODO don't do this for private channels in guilds
		initialState = append(initialState, &event.Event{
			Type: event.StateJoinRules,
			Content: event.Content{Parsed: &event.JoinRulesEventContent{
				JoinRule: event.JoinRuleRestricted,
				Allow: []event.JoinRuleAllow{{
					RoomID: spaceID,
					Type:   event.JoinRuleAllowRoomMembership,
				}},
			}},
		})
	}
	// TODO set restricted join rule based on guild

	var invite []id.UserID

	if portal.bridge.Config.Bridge.Encryption.Default {
		initialState = append(initialState, &event.Event{
			Type: event.StateEncryption,
			Content: event.Content{
				Parsed: event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1},
			},
		})
		portal.Encrypted = true

		if portal.IsPrivateChat() {
			invite = append(invite, portal.bridge.Bot.UserID)
		}
	}

	resp, err := intent.CreateRoom(&mautrix.ReqCreateRoom{
		Visibility:      "private",
		Name:            portal.Name,
		Topic:           portal.Topic,
		Invite:          invite,
		Preset:          "private_chat",
		IsDirect:        portal.IsPrivateChat(),
		InitialState:    initialState,
		CreationContent: creationContent,
	})
	if err != nil {
		portal.log.Warnln("Failed to create room:", err)
		return err
	}

	portal.NameSet = true
	portal.TopicSet = true
	portal.AvatarSet = !portal.AvatarURL.IsEmpty()
	portal.MXID = resp.RoomID
	portal.bridge.portalsLock.Lock()
	portal.bridge.portalsByMXID[portal.MXID] = portal
	portal.bridge.portalsLock.Unlock()
	portal.Update()
	portal.log.Infoln("Matrix room created:", portal.MXID)

	portal.updateSpace()
	portal.ensureUserInvited(user)
	user.syncChatDoublePuppetDetails(portal, true)

	portal.syncParticipants(user, channel.Recipients)

	if portal.IsPrivateChat() {
		puppet := user.bridge.GetPuppetByID(portal.Key.Receiver)

		chats := map[id.UserID][]id.RoomID{puppet.MXID: {portal.MXID}}
		user.updateDirectChats(chats)
	}

	firstEventResp, err := portal.MainIntent().SendMessageEvent(portal.MXID, portalCreationDummyEvent, struct{}{})
	if err != nil {
		portal.log.Errorln("Failed to send dummy event to mark portal creation:", err)
	} else {
		portal.FirstEventID = firstEventResp.EventID
		portal.Update()
	}

	return nil
}

func (portal *Portal) handleDiscordMessages(msg portalDiscordMessage) {
	if portal.MXID == "" {
		_, ok := msg.msg.(*discordgo.MessageCreate)
		if !ok {
			portal.log.Warnln("Can't create Matrix room from non new message event")
			return
		}

		portal.log.Debugln("Creating Matrix room from incoming message")
		if err := portal.CreateMatrixRoom(msg.user, nil); err != nil {
			portal.log.Errorln("Failed to create portal room:", err)
			return
		}
	}

	switch convertedMsg := msg.msg.(type) {
	case *discordgo.MessageCreate:
		portal.handleDiscordMessageCreate(msg.user, convertedMsg.Message, msg.thread)
	case *discordgo.MessageUpdate:
		portal.handleDiscordMessageUpdate(msg.user, convertedMsg.Message)
	case *discordgo.MessageDelete:
		portal.handleDiscordMessageDelete(msg.user, convertedMsg.Message)
	case *discordgo.MessageReactionAdd:
		portal.handleDiscordReaction(msg.user, convertedMsg.MessageReaction, true, msg.thread)
	case *discordgo.MessageReactionRemove:
		portal.handleDiscordReaction(msg.user, convertedMsg.MessageReaction, false, msg.thread)
	default:
		portal.log.Warnln("unknown message type")
	}
}

func (portal *Portal) ensureUserInvited(user *User) bool {
	return user.ensureInvited(portal.MainIntent(), portal.MXID, portal.IsPrivateChat())
}

func (portal *Portal) markMessageHandled(discordID string, mxid id.EventID, authorID string, timestamp time.Time, threadID string) *database.Message {
	msg := portal.bridge.DB.Message.New()
	msg.Channel = portal.Key
	msg.DiscordID = discordID
	msg.MXID = mxid
	msg.SenderID = authorID
	msg.Timestamp = timestamp
	msg.ThreadID = threadID
	msg.Insert()
	return msg
}

func (portal *Portal) sendMediaFailedMessage(intent *appservice.IntentAPI, bridgeErr error) {
	content := &event.MessageEventContent{
		Body:    fmt.Sprintf("Failed to bridge media: %v", bridgeErr),
		MsgType: event.MsgNotice,
	}

	_, err := portal.sendMatrixMessage(intent, event.EventMessage, content, nil, 0)
	if err != nil {
		portal.log.Warnfln("Failed to send media error message to matrix: %v", err)
	}
}

func (portal *Portal) handleDiscordAttachment(intent *appservice.IntentAPI, msgID string, attachment *discordgo.MessageAttachment, ts time.Time, threadRelation *event.RelatesTo, threadID string) {
	// var captionContent *event.MessageEventContent

	// if attachment.Description != "" {
	// 	captionContent = &event.MessageEventContent{
	// 		Body:    attachment.Description,
	// 		MsgType: event.MsgNotice,
	// 	}
	// }
	// portal.Log.Debugfln("captionContent: %#v", captionContent)

	content := &event.MessageEventContent{
		Body: attachment.Filename,
		Info: &event.FileInfo{
			Height:   attachment.Height,
			MimeType: attachment.ContentType,
			Width:    attachment.Width,

			// This gets overwritten later after the file is uploaded to the homeserver
			Size: attachment.Size,
		},
		RelatesTo: threadRelation,
	}

	switch strings.ToLower(strings.Split(attachment.ContentType, "/")[0]) {
	case "audio":
		content.MsgType = event.MsgAudio
	case "image":
		content.MsgType = event.MsgImage
	case "video":
		content.MsgType = event.MsgVideo
	default:
		content.MsgType = event.MsgFile
	}

	data, err := portal.downloadDiscordAttachment(attachment.URL)
	if err != nil {
		portal.sendMediaFailedMessage(intent, err)

		return
	}

	err = portal.uploadMatrixAttachment(intent, data, content)
	if err != nil {
		portal.sendMediaFailedMessage(intent, err)

		return
	}

	resp, err := portal.sendMatrixMessage(intent, event.EventMessage, content, nil, ts.UnixMilli())
	if err != nil {
		portal.log.Warnfln("failed to send media message to matrix: %v", err)
	}

	dbAttachment := portal.bridge.DB.Attachment.New()
	dbAttachment.Channel = portal.Key
	dbAttachment.MessageID = msgID
	dbAttachment.ID = attachment.ID
	dbAttachment.MXID = resp.EventID
	dbAttachment.ThreadID = threadID
	dbAttachment.Insert()
	// Update the fallback reply event for the next attachment
	if threadRelation != nil {
		threadRelation.InReplyTo.EventID = resp.EventID
	}
}

func (portal *Portal) handleDiscordMessageCreate(user *User, msg *discordgo.Message, thread *Thread) {
	if portal.MXID == "" {
		portal.log.Warnln("handle message called without a valid portal")

		return
	}

	// Handle room name changes
	if msg.Type == discordgo.MessageTypeChannelNameChange {
		//channel, err := user.Session.Channel(msg.ChannelID)
		//if err != nil {
		//	portal.log.Errorf("Failed to find the channel for portal %s", portal.Key)
		//	return
		//}
		//
		//name, err := portal.bridge.Config.Bridge.FormatChannelname(channel, user.Session)
		//if err != nil {
		//	portal.log.Errorf("Failed to format name for portal %s", portal.Key)
		//	return
		//}
		//
		//portal.Name = name
		//portal.Update()
		//
		//portal.MainIntent().SetRoomName(portal.MXID, name)

		return
	}

	// Handle normal message
	existing := portal.bridge.DB.Message.GetByDiscordID(portal.Key, msg.ID)
	if existing != nil {
		portal.log.Debugln("Dropping duplicate message", msg.ID)
		return
	}
	portal.log.Debugfln("Starting handling of %s by %s", msg.ID, msg.Author.ID)

	puppet := portal.bridge.GetPuppetByID(msg.Author.ID)
	puppet.UpdateInfo(user, msg.Author)
	intent := puppet.IntentFor(portal)

	var threadRelation *event.RelatesTo
	var threadID string
	if thread != nil {
		threadID = thread.ID
		lastEventID := thread.RootMXID
		lastInThread := portal.bridge.DB.Message.GetLastInThread(portal.Key, thread.ID)
		if lastInThread != nil {
			lastEventID = lastInThread.MXID
		}
		threadRelation = (&event.RelatesTo{}).SetThread(thread.RootMXID, lastEventID)
	}

	ts, _ := discordgo.SnowflakeTimestamp(msg.ID)
	if msg.Content != "" {
		content := &event.MessageEventContent{
			Body:      msg.Content,
			MsgType:   event.MsgText,
			RelatesTo: threadRelation.Copy(),
		}

		if msg.MessageReference != nil {
			//key := database.PortalKey{msg.MessageReference.ChannelID, user.ID}
			replyTo := portal.bridge.DB.Message.GetByDiscordID(portal.Key, msg.MessageReference.MessageID)
			if replyTo != nil {
				if content.RelatesTo == nil {
					content.RelatesTo = &event.RelatesTo{}
				}
				content.RelatesTo.SetReplyTo(replyTo.MXID)
			}
		}

		resp, err := portal.sendMatrixMessage(intent, event.EventMessage, content, nil, ts.UnixMilli())
		if err != nil {
			portal.log.Warnfln("failed to send message %q to matrix: %v", msg.ID, err)
			return
		}

		portal.markMessageHandled(msg.ID, resp.EventID, msg.Author.ID, ts, threadID)
		// Update the fallback reply event for attachments
		if threadRelation != nil {
			threadRelation.InReplyTo.EventID = resp.EventID
		}
	}

	for _, attachment := range msg.Attachments {
		portal.handleDiscordAttachment(intent, msg.ID, attachment, ts, threadRelation, threadID)
	}
}

func (portal *Portal) handleDiscordMessageUpdate(user *User, msg *discordgo.Message) {
	if portal.MXID == "" {
		portal.log.Warnln("handle message called without a valid portal")

		return
	}

	existing := portal.bridge.DB.Message.GetByDiscordID(portal.Key, msg.ID)
	if existing == nil {
		portal.log.Warnfln("Dropping update of unknown message %s", msg.ID)
		return
	}

	if msg.Flags == discordgo.MessageFlagsHasThread {
		portal.bridge.GetThreadByID(msg.ID, existing)
		portal.log.Debugfln("Marked %s as a thread root", msg.ID)
		// TODO make autojoining optional
		err := user.Session.ThreadJoinWithLocation(msg.ID, discordgo.ThreadJoinLocationContextMenu)
		if err != nil {
			user.log.Warnfln("Error autojoining thread %s@%s: %v", msg.ChannelID, portal.Key.ChannelID, err)
		}
	}

	// There's a few scenarios where the author is nil but I haven't figured
	// them all out yet.
	if msg.Author == nil {
		// If the server has to lookup opengraph previews it'll send the
		// message through without the preview and then add the preview later
		// via a message update. However, when it does this there is no author
		// as it's just the server, so for the moment we'll ignore this to
		// avoid a crash.
		if len(msg.Embeds) > 0 {
			portal.log.Debugln("ignoring update for opengraph attachment")

			return
		}

		portal.log.Errorfln("author is nil: %#v", msg)
		return
	}

	intent := portal.bridge.GetPuppetByID(msg.Author.ID).IntentFor(portal)

	if existing == nil {
		// Due to the differences in Discord and Matrix attachment handling,
		// existing will return nil if the original message was empty as we
		// don't store/save those messages so we can determine when we're
		// working against an attachment and do the attachment lookup instead.

		// Find all the existing attachments and drop them in a map so we can
		// figure out which, if any have been deleted and clean them up on the
		// matrix side.
		attachmentMap := map[string]*database.Attachment{}
		attachments := portal.bridge.DB.Attachment.GetAllByDiscordMessageID(portal.Key, msg.ID)

		for _, attachment := range attachments {
			attachmentMap[attachment.ID] = attachment
		}

		// Now run through the list of attachments on this message and remove
		// them from the map.
		for _, attachment := range msg.Attachments {
			if _, found := attachmentMap[attachment.ID]; found {
				delete(attachmentMap, attachment.ID)
			}
		}

		// Finally run through any attachments still in the map and delete them
		// on the matrix side and our database.
		for _, attachment := range attachmentMap {
			_, err := intent.RedactEvent(portal.MXID, attachment.MXID)
			if err != nil {
				portal.log.Warnfln("Failed to remove attachment %s: %v", attachment.MXID, err)
			}

			attachment.Delete()
		}

		return
	}

	content := &event.MessageEventContent{
		Body:    msg.Content,
		MsgType: event.MsgText,
	}
	content.SetEdit(existing.MXID)

	var editTS int64
	if msg.EditedTimestamp != nil {
		editTS = msg.EditedTimestamp.UnixMilli()
	}
	// TODO figure out some way to deduplicate outgoing edits
	_, err := portal.sendMatrixMessage(intent, event.EventMessage, content, nil, editTS)
	if err != nil {
		portal.log.Warnfln("failed to send message %q to matrix: %v", msg.ID, err)

		return
	}

	//ts, _ := msg.Timestamp.Parse()
	//portal.markMessageHandled(existing, msg.ID, resp.EventID, msg.Author.ID, ts)
}

func (portal *Portal) handleDiscordMessageDelete(user *User, msg *discordgo.Message) {
	// The discord delete message object is pretty empty and doesn't include
	// the author so we have to use the DMUser from the portal that was added
	// at creation time if we're a DM. We'll might have similar issues when we
	// add guild message support, but we'll cross that bridge when we get
	// there.

	// Find the message that we're working with. This could correctly return
	// nil if the message was just one or more attachments.
	existing := portal.bridge.DB.Message.GetByDiscordID(portal.Key, msg.ID)
	intent := portal.MainIntent()

	if existing != nil {
		attachments := portal.bridge.DB.Attachment.GetAllByDiscordMessageID(portal.Key, msg.ID)
		for _, attachment := range attachments {
			_, err := intent.RedactEvent(portal.MXID, attachment.MXID)
			if err != nil {
				portal.log.Warnfln("Failed to redact attachment %s: %v", attachment.MXID, err)
			}
			attachment.Delete()
		}

		_, err := intent.RedactEvent(portal.MXID, existing.MXID)
		if err != nil {
			portal.log.Warnfln("Failed to redact message %s: %v", existing.MXID, err)
		}
		existing.Delete()
	}
}

func (portal *Portal) syncParticipants(source *User, participants []*discordgo.User) {
	for _, participant := range participants {
		puppet := portal.bridge.GetPuppetByID(participant.ID)
		puppet.UpdateInfo(source, participant)

		user := portal.bridge.GetUserByID(participant.ID)
		if user != nil {
			portal.ensureUserInvited(user)
		}

		if user == nil || !puppet.IntentFor(portal).IsCustomPuppet {
			if err := puppet.IntentFor(portal).EnsureJoined(portal.MXID); err != nil {
				portal.log.Warnfln("Failed to make puppet of %s join %s: %v", participant.ID, portal.MXID, err)
			}
		}
	}
}

func (portal *Portal) encrypt(content *event.Content, eventType event.Type) (event.Type, error) {
	if portal.Encrypted && portal.bridge.Crypto != nil {
		// TODO maybe the locking should be inside mautrix-go?
		portal.encryptLock.Lock()
		encrypted, err := portal.bridge.Crypto.Encrypt(portal.MXID, eventType, *content)
		portal.encryptLock.Unlock()
		if err != nil {
			return eventType, fmt.Errorf("failed to encrypt event: %w", err)
		}
		eventType = event.EventEncrypted
		content.Parsed = encrypted
	}
	return eventType, nil
}

const doublePuppetValue = "mautrix-discord"

func (portal *Portal) sendMatrixMessage(intent *appservice.IntentAPI, eventType event.Type, content *event.MessageEventContent, extraContent map[string]interface{}, timestamp int64) (*mautrix.RespSendEvent, error) {
	wrappedContent := event.Content{Parsed: content, Raw: extraContent}
	if timestamp != 0 && intent.IsCustomPuppet {
		if wrappedContent.Raw == nil {
			wrappedContent.Raw = map[string]interface{}{}
		}
		if intent.IsCustomPuppet {
			wrappedContent.Raw[bridge.DoublePuppetKey] = doublePuppetValue
		}
	}
	var err error
	eventType, err = portal.encrypt(&wrappedContent, eventType)
	if err != nil {
		return nil, err
	}

	if eventType == event.EventEncrypted {
		// Clear other custom keys if the event was encrypted, but keep the double puppet identifier
		if intent.IsCustomPuppet {
			wrappedContent.Raw = map[string]interface{}{bridge.DoublePuppetKey: doublePuppetValue}
		} else {
			wrappedContent.Raw = nil
		}
	}

	_, _ = intent.UserTyping(portal.MXID, false, 0)
	if timestamp == 0 {
		return intent.SendMessageEvent(portal.MXID, eventType, &wrappedContent)
	} else {
		return intent.SendMassagedMessageEvent(portal.MXID, eventType, &wrappedContent, timestamp)
	}
}

func (portal *Portal) handleMatrixMessages(msg portalMatrixMessage) {
	switch msg.evt.Type {
	case event.EventMessage:
		portal.handleMatrixMessage(msg.user, msg.evt)
	case event.EventRedaction:
		portal.handleMatrixRedaction(msg.user, msg.evt)
	case event.EventReaction:
		portal.handleMatrixReaction(msg.user, msg.evt)
	default:
		portal.log.Debugln("unknown event type", msg.evt.Type)
	}
}

const discordEpoch = 1420070400000

func generateNonce() string {
	snowflake := (time.Now().UnixMilli() - discordEpoch) << 22
	// Nonce snowflakes don't have internal IDs or increments
	return strconv.FormatInt(snowflake, 10)
}

func (portal *Portal) getEvent(mxid id.EventID) (*event.Event, error) {
	evt, err := portal.MainIntent().GetEvent(portal.MXID, mxid)
	if err != nil {
		return nil, err
	}
	_ = evt.Content.ParseRaw(evt.Type)
	if evt.Type == event.EventEncrypted {
		decryptedEvt, err := portal.bridge.Crypto.Decrypt(evt)
		if err != nil {
			return nil, err
		} else {
			evt = decryptedEvt
		}
	}
	return evt, nil
}

func genThreadName(evt *event.Event) string {
	body := evt.Content.AsMessage().Body
	if len(body) == 0 {
		return "thread"
	}
	fields := strings.Fields(body)
	var title string
	for _, field := range fields {
		if len(title)+len(field) < 40 {
			title += field
			title += " "
			continue
		}
		if len(title) == 0 {
			title = field[:40]
		}
		break
	}
	return title
}

func (portal *Portal) startThreadFromMatrix(sender *User, threadRoot id.EventID) (string, error) {
	rootEvt, err := portal.getEvent(threadRoot)
	if err != nil {
		return "", fmt.Errorf("failed to get root event: %w", err)
	}
	threadName := genThreadName(rootEvt)

	existingMsg := portal.bridge.DB.Message.GetByMXID(portal.Key, threadRoot)
	if existingMsg == nil {
		return "", fmt.Errorf("unknown root event")
	} else if existingMsg.ThreadID != "" {
		return "", fmt.Errorf("root event is already in a thread")
	} else {
		var ch *discordgo.Channel
		ch, err = sender.Session.MessageThreadStartComplex(portal.Key.ChannelID, existingMsg.DiscordID, &discordgo.ThreadStart{
			Name:                threadName,
			AutoArchiveDuration: 24 * 60,
			Type:                discordgo.ChannelTypeGuildPublicThread,
			Location:            "Message",
		})
		if err != nil {
			return "", fmt.Errorf("error starting thread: %v", err)
		}
		portal.log.Debugfln("Created Discord thread from %s/%s", threadRoot, ch.ID)
		fmt.Printf("Created thread %+v\n", ch)
		portal.bridge.GetThreadByID(existingMsg.DiscordID, existingMsg)
		return ch.ID, nil
	}
}

func (portal *Portal) handleMatrixMessage(sender *User, evt *event.Event) {
	if portal.IsPrivateChat() && sender.DiscordID != portal.Key.Receiver {
		return
	}

	content, ok := evt.Content.Parsed.(*event.MessageEventContent)
	if !ok {
		portal.log.Debugfln("Failed to handle event %s: unexpected parsed content type %T", evt.ID, evt.Content.Parsed)
		return
	}

	channelID := portal.Key.ChannelID
	var threadID string

	if editMXID := content.GetRelatesTo().GetReplaceID(); editMXID != "" && content.NewContent != nil {
		edits := portal.bridge.DB.Message.GetByMXID(portal.Key, editMXID)
		if edits != nil {
			// we don't have anything to save for the update message right now
			// as we're not tracking edited timestamps.
			_, err := sender.Session.ChannelMessageEdit(edits.DiscordProtoChannelID(), edits.DiscordID, content.NewContent.Body)
			if err != nil {
				portal.log.Errorln("Failed to update message %s: %v", edits.DiscordID, err)
			}
		}
		return
	} else if threadRoot := content.GetRelatesTo().GetThreadParent(); threadRoot != "" {
		existingThread := portal.bridge.DB.Thread.GetByMatrixRootMsg(threadRoot)
		if existingThread != nil {
			threadID = existingThread.ID
		} else {
			var err error
			threadID, err = portal.startThreadFromMatrix(sender, threadRoot)
			if err != nil {
				portal.log.Warnfln("Failed to start thread from %s: %v", threadRoot, err)
			}
		}
	}
	if threadID != "" {
		channelID = threadID
	}

	var sendReq discordgo.MessageSend

	switch content.MsgType {
	case event.MsgText, event.MsgEmote, event.MsgNotice:
		if replyToMXID := content.GetReplyTo(); replyToMXID != "" {
			replyTo := portal.bridge.DB.Message.GetByMXID(portal.Key, replyToMXID)
			if replyTo != nil && replyTo.ThreadID == threadID {
				sendReq.Reference = &discordgo.MessageReference{
					ChannelID: channelID,
					MessageID: replyTo.DiscordID,
				}
			}
		}
		sendReq.Content = content.Body
	case event.MsgAudio, event.MsgFile, event.MsgImage, event.MsgVideo:
		data, err := portal.downloadMatrixAttachment(evt.ID, content)
		if err != nil {
			portal.log.Errorfln("Failed to download matrix attachment: %v", err)

			return
		}

		sendReq.Files = []*discordgo.File{{
			Name:        content.Body,
			ContentType: content.Info.MimeType,
			Reader:      bytes.NewReader(data),
		}}
	default:
		portal.log.Warnln("Unknown message type", content.MsgType)
		return
	}
	sendReq.Nonce = generateNonce()
	msg, err := sender.Session.ChannelMessageSendComplex(channelID, &sendReq)
	if err != nil {
		portal.log.Errorfln("Failed to send message: %v", err)
		return
	}

	if msg != nil {
		dbMsg := portal.bridge.DB.Message.New()
		dbMsg.Channel = portal.Key
		dbMsg.DiscordID = msg.ID
		dbMsg.MXID = evt.ID
		dbMsg.SenderID = sender.DiscordID
		dbMsg.Timestamp, _ = discordgo.SnowflakeTimestamp(msg.ID)
		dbMsg.ThreadID = threadID
		dbMsg.Insert()
	}
}

func (portal *Portal) HandleMatrixLeave(brSender bridge.User) {
	portal.log.Debugln("User left private chat portal, cleaning up and deleting...")
	portal.Delete()
	portal.cleanup(false)

	// TODO: figure out how to close a dm from the API.

	portal.cleanupIfEmpty()
}

func (portal *Portal) leave(sender *User) {
	if portal.MXID == "" {
		return
	}

	intent := portal.bridge.GetPuppetByID(sender.DiscordID).IntentFor(portal)
	intent.LeaveRoom(portal.MXID)
}

func (portal *Portal) Delete() {
	portal.Portal.Delete()
	portal.bridge.portalsLock.Lock()
	delete(portal.bridge.portalsByID, portal.Key)

	if portal.MXID != "" {
		delete(portal.bridge.portalsByMXID, portal.MXID)
	}

	portal.bridge.portalsLock.Unlock()
}

func (portal *Portal) cleanupIfEmpty() {
	if portal.MXID == "" {
		return
	}

	users, err := portal.getMatrixUsers()
	if err != nil {
		portal.log.Errorfln("Failed to get Matrix user list to determine if portal needs to be cleaned up: %v", err)
		return
	}

	if len(users) == 0 {
		portal.log.Infoln("Room seems to be empty, cleaning up...")
		portal.Delete()
		portal.cleanup(false)
	}
}

func (portal *Portal) cleanup(puppetsOnly bool) {
	if portal.MXID == "" {
		return
	}

	if portal.IsPrivateChat() {
		_, err := portal.MainIntent().LeaveRoom(portal.MXID)
		if err != nil {
			portal.log.Warnln("Failed to leave private chat portal with main intent:", err)
		}
		return
	}

	intent := portal.MainIntent()
	members, err := intent.JoinedMembers(portal.MXID)
	if err != nil {
		portal.log.Errorln("Failed to get portal members for cleanup:", err)
		return
	}

	for member := range members.Joined {
		if member == intent.UserID {
			continue
		}

		puppet := portal.bridge.GetPuppetByMXID(member)
		if portal != nil {
			_, err = puppet.DefaultIntent().LeaveRoom(portal.MXID)
			if err != nil {
				portal.log.Errorln("Error leaving as puppet while cleaning up portal:", err)
			}
		} else if !puppetsOnly {
			_, err = intent.KickUser(portal.MXID, &mautrix.ReqKickUser{UserID: member, Reason: "Deleting portal"})
			if err != nil {
				portal.log.Errorln("Error kicking user while cleaning up portal:", err)
			}
		}
	}

	_, err = intent.LeaveRoom(portal.MXID)
	if err != nil {
		portal.log.Errorln("Error leaving with main intent while cleaning up portal:", err)
	}
}

func (portal *Portal) getMatrixUsers() ([]id.UserID, error) {
	members, err := portal.MainIntent().JoinedMembers(portal.MXID)
	if err != nil {
		return nil, fmt.Errorf("failed to get member list: %w", err)
	}

	var users []id.UserID
	for userID := range members.Joined {
		_, isPuppet := portal.bridge.ParsePuppetMXID(userID)
		if !isPuppet && userID != portal.bridge.Bot.UserID {
			users = append(users, userID)
		}
	}

	return users, nil
}

func (portal *Portal) handleMatrixReaction(sender *User, evt *event.Event) {
	if portal.IsPrivateChat() && sender.DiscordID != portal.Key.Receiver {
		return
	}

	reaction := evt.Content.AsReaction()
	if reaction.RelatesTo.Type != event.RelAnnotation {
		portal.log.Errorfln("Ignoring reaction %s due to unknown m.relates_to data", evt.ID)
		return
	}

	var discordID, threadID string
	channelID := portal.Key.ChannelID

	msg := portal.bridge.DB.Message.GetByMXID(portal.Key, reaction.RelatesTo.EventID)

	// Due to the differences in attachments between Discord and Matrix, if a
	// user reacts to a media message on discord our lookup above will fail
	// because the relation of matrix media messages to attachments in handled
	// in the attachments table instead of messages so we need to check that
	// before continuing.
	//
	// This also leads to interesting problems when a Discord message comes in
	// with multiple attachments. A user can react to each one individually on
	// Matrix, which will cause us to send it twice. Discord tends to ignore
	// this, but if the user removes one of them, discord removes it and now
	// they're out of sync. Perhaps we should add a counter to the reactions
	// table to keep them in sync and to avoid sending duplicates to Discord.
	if msg == nil {
		attachment := portal.bridge.DB.Attachment.GetByMatrixID(portal.Key, reaction.RelatesTo.EventID)
		if attachment == nil {
			return
		}
		discordID = attachment.MessageID
		threadID = attachment.ThreadID
	} else {
		discordID = msg.DiscordID
		threadID = msg.ThreadID
	}

	// Figure out if this is a custom emoji or not.
	emojiID := reaction.RelatesTo.Key
	if strings.HasPrefix(emojiID, "mxc://") {
		uri, _ := id.ParseContentURI(emojiID)
		emoji := portal.bridge.DB.Emoji.GetByMatrixURL(uri)
		if emoji == nil {
			portal.log.Errorfln("Couldn't find emoji corresponding to %s", emojiID)
			return
		}

		emojiID = emoji.APIName()
	} else {
		emojiID = variationselector.Remove(emojiID)
	}

	if threadID != "" {
		channelID = threadID
	}
	err := sender.Session.MessageReactionAdd(channelID, discordID, emojiID)
	if err != nil {
		portal.log.Debugf("Failed to send reaction to %s: %v", discordID, err)
		return
	}

	dbReaction := portal.bridge.DB.Reaction.New()
	dbReaction.Channel = portal.Key
	dbReaction.MessageID = discordID
	dbReaction.Sender = sender.DiscordID
	dbReaction.EmojiName = emojiID
	dbReaction.ThreadID = threadID
	dbReaction.MXID = evt.ID
	dbReaction.Insert()
}

func (portal *Portal) handleDiscordReaction(user *User, reaction *discordgo.MessageReaction, add bool, thread *Thread) {
	intent := portal.bridge.GetPuppetByID(reaction.UserID).IntentFor(portal)

	var discordID string
	var matrixReaction string

	if reaction.Emoji.ID != "" {
		dbEmoji := portal.bridge.DB.Emoji.GetByDiscordID(reaction.Emoji.ID)

		if dbEmoji == nil {
			data, mimeType, err := portal.downloadDiscordEmoji(reaction.Emoji.ID, reaction.Emoji.Animated)
			if err != nil {
				portal.log.Warnfln("Failed to download emoji %s from discord: %v", reaction.Emoji.ID, err)

				return
			}

			uri, err := portal.uploadMatrixEmoji(intent, data, mimeType)
			if err != nil {
				portal.log.Warnfln("Failed to upload discord emoji %s to homeserver: %v", reaction.Emoji.ID, err)

				return
			}

			dbEmoji = portal.bridge.DB.Emoji.New()
			dbEmoji.DiscordID = reaction.Emoji.ID
			dbEmoji.DiscordName = reaction.Emoji.Name
			dbEmoji.MatrixURL = uri
			dbEmoji.Insert()
		}

		discordID = dbEmoji.DiscordID
		matrixReaction = dbEmoji.MatrixURL.String()
	} else {
		discordID = reaction.Emoji.Name
		matrixReaction = variationselector.Add(reaction.Emoji.Name)
	}

	// Find the message that we're working with.
	message := portal.bridge.DB.Message.GetByDiscordID(portal.Key, reaction.MessageID)
	if message == nil {
		portal.log.Debugfln("failed to add reaction to message %s: message not found", reaction.MessageID)

		return
	}

	// Lookup an existing reaction
	existing := portal.bridge.DB.Reaction.GetByDiscordID(portal.Key, message.DiscordID, reaction.UserID, discordID)
	if !add {
		if existing == nil {
			portal.log.Debugln("Failed to remove reaction for unknown message", reaction.MessageID)
			return
		}

		_, err := intent.RedactEvent(portal.MXID, existing.MXID)
		if err != nil {
			portal.log.Warnfln("Failed to remove reaction from %s: %v", portal.MXID, err)
		}

		existing.Delete()

		return
	} else if existing != nil {
		portal.log.Debugfln("Ignoring duplicate reaction %s from %s to %s", discordID, reaction.UserID, message.DiscordID)
		return
	}

	content := event.Content{Parsed: &event.ReactionEventContent{
		RelatesTo: event.RelatesTo{
			EventID: message.MXID,
			Type:    event.RelAnnotation,
			Key:     matrixReaction,
		},
	}}
	if intent.IsCustomPuppet {
		content.Raw = map[string]interface{}{
			bridge.DoublePuppetKey: doublePuppetValue,
		}
	}

	resp, err := intent.SendMessageEvent(portal.MXID, event.EventReaction, &content)
	if err != nil {
		portal.log.Errorfln("failed to send reaction from %s: %v", reaction.MessageID, err)

		return
	}

	if existing == nil {
		dbReaction := portal.bridge.DB.Reaction.New()
		dbReaction.Channel = portal.Key
		dbReaction.MessageID = message.DiscordID
		dbReaction.Sender = reaction.UserID
		dbReaction.EmojiName = discordID
		dbReaction.MXID = resp.EventID
		if thread != nil {
			dbReaction.ThreadID = thread.ID
		}
		dbReaction.Insert()
	}
}

func (portal *Portal) handleMatrixRedaction(user *User, evt *event.Event) {
	if user.DiscordID != portal.Key.Receiver {
		return
	}

	// First look if we're redacting a message
	message := portal.bridge.DB.Message.GetByMXID(portal.Key, evt.Redacts)
	if message != nil {
		err := user.Session.ChannelMessageDelete(message.DiscordProtoChannelID(), message.DiscordID)
		if err != nil {
			portal.log.Debugfln("Failed to delete discord message %s: %v", message.DiscordID, err)
		} else {
			message.Delete()
		}
		return
	}

	// Now check if it's a reaction.
	reaction := portal.bridge.DB.Reaction.GetByMXID(evt.Redacts)
	if reaction != nil && reaction.Channel == portal.Key {
		err := user.Session.MessageReactionRemove(reaction.DiscordProtoChannelID(), reaction.MessageID, reaction.EmojiName, reaction.Sender)
		if err != nil {
			portal.log.Debugfln("Failed to delete reaction %s from %s: %v", reaction.EmojiName, reaction.MessageID, err)
		} else {
			reaction.Delete()
		}

		return
	}

	portal.log.Warnfln("Failed to redact %s: no event found", evt.Redacts)
}

func (portal *Portal) UpdateName(name string) bool {
	if portal.Name == name && portal.NameSet {
		return false
	} else if !portal.Encrypted && portal.IsPrivateChat() {
		// TODO custom config option for always setting private chat portal meta?
		return false
	}
	portal.Name = name
	portal.NameSet = false
	if portal.MXID != "" {
		_, err := portal.MainIntent().SetRoomName(portal.MXID, portal.Name)
		if err != nil {
			portal.log.Warnln("Failed to update room name:", err)
		} else {
			portal.NameSet = true
		}
	}
	return true
}

func (portal *Portal) UpdateAvatarFromPuppet(puppet *Puppet) bool {
	if portal.Avatar == puppet.Avatar && portal.AvatarSet {
		return false
	}
	portal.Avatar = puppet.Avatar
	portal.AvatarURL = puppet.AvatarURL
	portal.AvatarSet = false
	portal.updateRoomAvatar()
	return true
}

func (portal *Portal) UpdateGroupDMAvatar(iconID string) bool {
	if portal.Avatar == iconID && portal.AvatarSet {
		return false
	}
	portal.Avatar = iconID
	portal.AvatarSet = false
	if portal.Avatar != "" {
		uri, err := uploadAvatar(portal.MainIntent(), discordgo.EndpointGroupIcon(portal.Key.ChannelID, portal.Avatar))
		if err != nil {
			portal.log.Warnln("Failed to reupload avatar:", err)
			return true
		} else {
			portal.AvatarURL = uri
		}
	} else {
		portal.AvatarURL = id.ContentURI{}
	}
	portal.updateRoomAvatar()
	return true
}

func (portal *Portal) updateRoomAvatar() {
	if portal.MXID == "" {
		return
	}
	_, err := portal.MainIntent().SetRoomAvatar(portal.MXID, portal.AvatarURL)
	if err != nil {
		portal.log.Warnln("Failed to update room avatar:", err)
	} else {
		portal.AvatarSet = true
	}
}

func (portal *Portal) UpdateTopic(topic string) bool {
	if portal.Topic == topic && portal.TopicSet {
		return false
	}
	portal.Topic = topic
	portal.TopicSet = false
	if portal.MXID != "" {
		_, err := portal.MainIntent().SetRoomTopic(portal.MXID, portal.Topic)
		if err != nil {
			portal.log.Warnln("Failed to update room topic:", err)
		}
	}
	return true
}

func (portal *Portal) removeFromSpace() {
	if portal.InSpace == "" {
		return
	}

	_, err := portal.MainIntent().SendStateEvent(portal.MXID, event.StateSpaceParent, portal.InSpace.String(), struct{}{})
	if err != nil {
		portal.log.Warnfln("Failed to unset canonical space %s: %v", portal.InSpace, err)
	}
	_, err = portal.bridge.Bot.SendStateEvent(portal.InSpace, event.StateSpaceChild, portal.MXID.String(), struct{}{})
	if err != nil {
		portal.log.Warnfln("Failed to add room to space %s: %v", portal.InSpace, err)
	}
	portal.InSpace = ""
}

func (portal *Portal) addToSpace(mxid id.RoomID) bool {
	if portal.InSpace == mxid {
		return false
	}
	portal.removeFromSpace()

	_, err := portal.MainIntent().SendStateEvent(portal.MXID, event.StateSpaceParent, mxid.String(), &event.SpaceParentEventContent{
		Via:       []string{portal.bridge.AS.HomeserverDomain},
		Canonical: true,
	})
	if err != nil {
		portal.log.Warnfln("Failed to set canonical space %s: %v", mxid, err)
	}

	_, err = portal.bridge.Bot.SendStateEvent(mxid, event.StateSpaceChild, portal.MXID.String(), &event.SpaceChildEventContent{
		Via: []string{portal.bridge.AS.HomeserverDomain},
		// TODO order
	})
	if err != nil {
		portal.log.Warnfln("Failed to add room to space %s: %v", mxid, err)
	} else {
		portal.InSpace = mxid
	}
	return true
}

func (portal *Portal) UpdateParent(parentID string) bool {
	if portal.ParentID == parentID {
		return false
	}
	portal.ParentID = parentID
	if portal.ParentID != "" {
		portal.Parent = portal.bridge.GetExistingPortalByID(database.NewPortalKey(parentID, ""))
	} else {
		portal.Parent = nil
	}
	return true
}

func (portal *Portal) ExpectedSpaceID() id.RoomID {
	if portal.Parent != nil {
		return portal.Parent.MXID
	} else if portal.Guild != nil {
		return portal.Guild.MXID
	}
	return ""
}

func (portal *Portal) updateSpace() bool {
	if portal.MXID == "" {
		return false
	}
	if portal.Parent != nil {
		return portal.addToSpace(portal.Parent.MXID)
	} else if portal.Guild != nil {
		return portal.addToSpace(portal.Guild.MXID)
	}
	return false
}

func (portal *Portal) UpdateInfo(source *User, meta *discordgo.Channel) *discordgo.Channel {
	changed := false

	if portal.Type != meta.Type {
		portal.log.Warnfln("Portal type changed from %d to %d", portal.Type, meta.Type)
		portal.Type = meta.Type
		changed = true
	}
	if portal.OtherUserID == "" && portal.IsPrivateChat() {
		if len(meta.Recipients) == 0 {
			var err error
			meta, err = source.Session.Channel(meta.ID)
			if err != nil {
				portal.log.Errorfln("Failed to get DM channel info:", err)
			}
		}
		portal.OtherUserID = meta.Recipients[0].ID
		portal.log.Infoln("Found other user ID:", portal.OtherUserID)
		changed = true
	}
	if meta.GuildID != "" && portal.GuildID == "" {
		portal.GuildID = meta.GuildID
		portal.Guild = portal.bridge.GetGuildByID(portal.GuildID, true)
		changed = true
	}

	// FIXME
	//name, err := portal.bridge.Config.Bridge.FormatChannelname(meta, source.Session)
	//if err != nil {
	//	portal.log.Errorln("Failed to format channel name:", err)
	//	return
	//}

	switch portal.Type {
	case discordgo.ChannelTypeDM:
		if portal.OtherUserID != "" {
			puppet := portal.bridge.GetPuppetByID(portal.OtherUserID)
			changed = portal.UpdateAvatarFromPuppet(puppet) || changed
			changed = portal.UpdateName(puppet.Name) || changed
		}
	case discordgo.ChannelTypeGroupDM:
		changed = portal.UpdateGroupDMAvatar(meta.Icon) || changed
		fallthrough
	default:
		changed = portal.UpdateName(meta.Name) || changed
	}
	changed = portal.UpdateTopic(meta.Topic) || changed
	changed = portal.UpdateParent(meta.ParentID) || changed
	if portal.MXID != "" && portal.ExpectedSpaceID() != portal.InSpace {
		changed = portal.updateSpace() || changed
	}
	if changed {
		portal.UpdateBridgeInfo()
		portal.Update()
	}
	return meta
}