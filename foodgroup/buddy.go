package foodgroup

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/mk6i/open-oscar-server/state"
	"github.com/mk6i/open-oscar-server/wire"
)

func dedupeBuddyAdds(instance *state.SessionInstance, inBody wire.SNAC_0x03_0x04_BuddyAddBuddies) []state.IdentScreenName {
	me := instance.IdentScreenName()
	seen := make(map[string]struct{}, len(inBody.Buddies))
	out := make([]state.IdentScreenName, 0, len(inBody.Buddies))
	for _, entry := range inBody.Buddies {
		sn := state.NormalizeICQUINBuddyKey(state.NewIdentScreenName(entry.ScreenName))
		if sn == me {
			continue
		}
		key := sn.String()
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, sn)
	}
	return out
}

func dedupeBuddyDels(instance *state.SessionInstance, inBody wire.SNAC_0x03_0x05_BuddyDelBuddies) []state.IdentScreenName {
	me := instance.IdentScreenName()
	seen := make(map[string]struct{}, len(inBody.Buddies))
	out := make([]state.IdentScreenName, 0, len(inBody.Buddies))
	for _, entry := range inBody.Buddies {
		sn := state.NormalizeICQUINBuddyKey(state.NewIdentScreenName(entry.ScreenName))
		if sn == me {
			continue
		}
		key := sn.String()
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, sn)
	}
	return out
}

// NewBuddyService creates a new instance of BuddyService.
func NewBuddyService(
	logger *slog.Logger,
	messageRelayer MessageRelayer,
	clientSideBuddyListManager ClientSideBuddyListManager,
	relationshipFetcher RelationshipFetcher,
	sessionRetriever SessionRetriever,
	bartItemManager BARTItemManager,
	buddyUserLookup BuddyFeedbagUserLookup,
) *BuddyService {
	return &BuddyService{
		buddyBroadcaster: newBuddyNotifier(
			logger,
			bartItemManager,
			relationshipFetcher,
			messageRelayer,
			sessionRetriever,
			buddyUserLookup,
		),
		clientSideBuddyListManager: clientSideBuddyListManager,
	}
}

// BuddyService provides functionality for the Buddy food group.
type BuddyService struct {
	clientSideBuddyListManager ClientSideBuddyListManager
	buddyBroadcaster           buddyBroadcaster
}

// BuddyWatcherListQuery answers SNAC(0x03,0x06). ICQ 6 expects a valid
// BuddyWatcherListResponse; replying with SNAC error breaks presence.
func (s *BuddyService) BuddyWatcherListQuery(_ context.Context, frameIn wire.SNACFrame) wire.SNACMessage {
	return wire.SNACMessage{
		Frame: wire.SNACFrame{
			FoodGroup: wire.Buddy,
			SubGroup:  wire.BuddyWatcherListResponse,
			RequestID: frameIn.RequestID,
		},
		Body: wire.SNAC_0x03_0x07_BuddyWatcherListResponse{
			WatcherCount: 0,
		},
	}
}

// BuddyWatcherSubRequest handles SNAC(0x03,0x08). After subscribing, ICQ 6 is ready
// to apply BuddyArrived updates; refresh visibility so online buddies are pushed.
func (s *BuddyService) BuddyWatcherSubRequest(ctx context.Context, instance *state.SessionInstance, _ wire.SNACFrame, r io.Reader) error {
	inBody := wire.SNAC_0x03_0x08_BuddyWatcherSubRequest{}
	if err := wire.UnmarshalBE(&inBody, r); err != nil {
		return err
	}
	filter := make([]state.IdentScreenName, 0, len(inBody.Buddies))
	for _, b := range inBody.Buddies {
		filter = append(filter, state.NormalizeICQUINBuddyKey(state.NewIdentScreenName(b.ScreenName)))
	}
	if len(filter) == 0 {
		return s.buddyBroadcaster.BroadcastVisibility(ctx, instance, nil, false)
	}
	return s.buddyBroadcaster.BroadcastVisibility(ctx, instance, filter, false)
}

// RightsQuery returns buddy list service parameters.
func (s BuddyService) RightsQuery(_ context.Context, frameIn wire.SNACFrame) wire.SNACMessage {
	return wire.SNACMessage{
		Frame: wire.SNACFrame{
			FoodGroup: wire.Buddy,
			SubGroup:  wire.BuddyRightsReply,
			RequestID: frameIn.RequestID,
		},
		Body: wire.SNAC_0x03_0x03_BuddyRightsReply{
			TLVRestBlock: wire.TLVRestBlock{
				TLVList: wire.TLVList{
					wire.NewTLVBE(wire.BuddyTLVTagsParmMaxBuddies, uint16(100)),
					wire.NewTLVBE(wire.BuddyTLVTagsParmMaxWatchers, uint16(100)),
					wire.NewTLVBE(wire.BuddyTLVTagsParmMaxIcqBroad, uint16(100)),
					wire.NewTLVBE(wire.BuddyTLVTagsParmMaxTempBuddies, uint16(100)),
				},
			},
		},
	}
}

// AddBuddies adds buddies to my client-side buddy list.
func (s BuddyService) AddBuddies(ctx context.Context, instance *state.SessionInstance, inBody wire.SNAC_0x03_0x04_BuddyAddBuddies) error {
	toAdd := dedupeBuddyAdds(instance, inBody)
	for _, sn := range toAdd {
		if err := s.clientSideBuddyListManager.AddBuddy(ctx, instance.IdentScreenName(), sn); err != nil {
			return err
		}
	}

	if !instance.SignonComplete() {
		// client has not completed sign-on sequence, so any arrival
		// messages sent at this point would be ignored by the client.
		return nil
	}

	if len(toAdd) == 0 {
		return nil
	}
	if err := s.buddyBroadcaster.BroadcastVisibility(ctx, instance, toAdd, true); err != nil {
		return fmt.Errorf("buddyBroadcaster.BroadcastVisibility: %w", err)
	}

	return nil
}

// DelBuddies deletes buddies from my client-side buddy list.
func (s BuddyService) DelBuddies(ctx context.Context, instance *state.SessionInstance, inBody wire.SNAC_0x03_0x05_BuddyDelBuddies) error {
	toDel := dedupeBuddyDels(instance, inBody)
	for _, sn := range toDel {
		if err := s.clientSideBuddyListManager.RemoveBuddy(ctx, instance.IdentScreenName(), sn); err != nil {
			return err
		}
	}

	if len(toDel) == 0 {
		return nil
	}
	if err := s.buddyBroadcaster.BroadcastVisibility(ctx, instance, toDel, true); err != nil {
		return fmt.Errorf("buddyBroadcaster.BroadcastVisibility: %w", err)
	}

	return nil
}

// AddTempBuddies adds temporary buddies to the user's buddy list that persist
// for the duration of the user's session.
func (s BuddyService) AddTempBuddies(ctx context.Context, instance *state.SessionInstance, inBody wire.SNAC_0x03_0x0F_BuddyAddTempBuddies) error {
	var b wire.SNAC_0x03_0x04_BuddyAddBuddies

	for _, buddy := range inBody.Buddies {
		b.Buddies = append(b.Buddies, struct {
			ScreenName string `oscar:"len_prefix=uint8"`
		}{ScreenName: buddy.ScreenName})
	}

	return s.AddBuddies(ctx, instance, b)
}

// DelTempBuddies deletes temporary buddies from the user's buddy list.
func (s BuddyService) DelTempBuddies(ctx context.Context, instance *state.SessionInstance, inBody wire.SNAC_0x03_0x10_BuddyDelTempBuddies) error {
	var b wire.SNAC_0x03_0x05_BuddyDelBuddies

	for _, buddy := range inBody.Buddies {
		b.Buddies = append(b.Buddies, struct {
			ScreenName string `oscar:"len_prefix=uint8"`
		}{ScreenName: buddy.ScreenName})
	}

	return s.DelBuddies(ctx, instance, b)
}

// BroadcastBuddyArrived broadcasts buddy arrival with custom user info (implements DepartureNotifier)
func (s BuddyService) BroadcastBuddyArrived(ctx context.Context, screenName state.IdentScreenName, userInfo wire.TLVUserInfo) error {
	return s.buddyBroadcaster.BroadcastBuddyArrived(ctx, screenName, userInfo)
}

func (s BuddyService) BroadcastBuddyDeparted(ctx context.Context, screenName state.IdentScreenName) error {
	return s.buddyBroadcaster.BroadcastBuddyDeparted(ctx, screenName)
}

func (s BuddyService) BroadcastVisibility(ctx context.Context, you *state.SessionInstance, filter []state.IdentScreenName, doSendDepartures bool) error {
	return s.buddyBroadcaster.BroadcastVisibility(ctx, you, filter, doSendDepartures)
}

func newBuddyNotifier(
	logger *slog.Logger,
	bartItemManager BARTItemManager,
	relationshipFetcher RelationshipFetcher,
	messageRelayer MessageRelayer,
	sessionRetriever SessionRetriever,
	buddyUserLookup BuddyFeedbagUserLookup,
) buddyNotifier {
	return buddyNotifier{
		logger:              logger,
		bartItemManager:     bartItemManager,
		relationshipFetcher: relationshipFetcher,
		messageRelayer:      messageRelayer,
		sessionRetriever:    sessionRetriever,
		buddyUserLookup:     buddyUserLookup,
	}
}

// buddyNotifier centralizes logic for sending buddy arrival and departure
// notifications.
type buddyNotifier struct {
	logger              *slog.Logger
	bartItemManager     BARTItemManager
	relationshipFetcher RelationshipFetcher
	messageRelayer      MessageRelayer
	sessionRetriever    SessionRetriever
	buddyUserLookup     BuddyFeedbagUserLookup
}

func (s buddyNotifier) retrieveLiveBuddySession(ctx context.Context, feedbagKey state.IdentScreenName) *state.Session {
	if s.sessionRetriever == nil {
		return nil
	}
	if sess := s.sessionRetriever.RetrieveSession(feedbagKey); sess != nil {
		return sess
	}
	if norm := state.NormalizeICQUINBuddyKey(feedbagKey); norm.String() != feedbagKey.String() {
		if sess := s.sessionRetriever.RetrieveSession(norm); sess != nil {
			return sess
		}
	}
	if s.buddyUserLookup == nil {
		return nil
	}
	u, err := s.buddyUserLookup.UserForFeedbagBuddyKey(ctx, feedbagKey)
	if err != nil || u == nil {
		return nil
	}
	return s.sessionRetriever.RetrieveSession(u.IdentScreenName)
}

func (s buddyNotifier) canonicalBuddyIdent(ctx context.Context, feedbagKey state.IdentScreenName) state.IdentScreenName {
	if s.buddyUserLookup == nil {
		return feedbagKey
	}
	u, err := s.buddyUserLookup.UserForFeedbagBuddyKey(ctx, feedbagKey)
	if err != nil || u == nil {
		return feedbagKey
	}
	return u.IdentScreenName
}

// BroadcastBuddyArrived sends the latest user info to the user's adjacent users.
// While updates are sent via the wire.BuddyArrived SNAC, the message is not
// only used to indicate the user coming online. It can also notify changes to
// buddy icons, warning levels, invisibility status, etc.
func (s buddyNotifier) BroadcastBuddyArrived(ctx context.Context, screenName state.IdentScreenName, userInfo wire.TLVUserInfo) error {
	if userInfo.IsInvisible() {
		return nil
	}

	users, err := s.relationshipFetcher.AllRelationships(ctx, screenName, nil)
	if err != nil {
		return err
	}

	var recipients []state.IdentScreenName
	for _, user := range users {
		if user.YouBlock || user.BlocksYou || !user.IsOnTheirList {
			continue
		}
		recipients = append(recipients, s.canonicalBuddyIdent(ctx, user.User))
	}

	for _, recip := range recipients {
		ui := s.buddyArrivedTLVForRecipient(ctx, recip, screenName, userInfo)
		s.messageRelayer.RelayToScreenName(ctx, recip, wire.SNACMessage{
			Frame: wire.SNACFrame{
				FoodGroup: wire.Buddy,
				SubGroup:  wire.BuddyArrived,
				RequestID: wire.ReqIDFromServer,
			},
			Body: wire.SNAC_0x03_0x0B_BuddyArrived{
				TLVUserInfo: ui,
			},
		})
	}

	return nil
}

// buddyArrivedTLVForRecipient returns the TLVUserInfo to embed in a BuddyArrived
// message for `recipient` when the buddy is `about`. It enriches the block with
// ICQ-specific TLVs (DC info, OscarCaps, UserFlags2, ICQ external IP) only when
// BOTH sides are ICQ:
//   - the buddy snapshot (userInfo) carries the ICQ user flag (0x0040), and
//   - the recipient session has the ICQ user flag set on at least one instance
//     (or is flagged as an ICQ account at the session level).
//
// AIM-only recipients always receive the unmodified userInfo. No client-id
// detection (e.g. "ICQ6 generation") is performed; the user-flag bitmask is the
// authoritative signal.
func (s buddyNotifier) buddyArrivedTLVForRecipient(ctx context.Context, recipient, about state.IdentScreenName, userInfo wire.TLVUserInfo) wire.TLVUserInfo {
	if s.sessionRetriever == nil {
		return userInfo
	}
	if !tlvUserInfoHasICQFlag(userInfo) {
		return userInfo
	}
	recipSess := s.sessionRetriever.RetrieveSession(recipient)
	if recipSess == nil {
		return userInfo
	}
	if !sessionHasICQFlag(recipSess) {
		return userInfo
	}

	// Recipient is confirmed ICQ. Look up the live buddy session so we can
	// pull DC info / BOS advert when enriching. Session keys are
	// canonical numeric UINs (see unicastBuddyArrived); callers may pass a
	// non-normalized IdentScreenName for the same account.
	aboutKey := state.NormalizeICQUINBuddyKey(about)
	buddySess := s.sessionRetriever.RetrieveSession(aboutKey)
	if buddySess == nil {
		buddySess = s.retrieveLiveBuddySession(ctx, aboutKey)
	}
	var bos string
	if buddySess != nil {
		bos = buddySess.ICQBOSAdvertisedHostPort()
	}
	return maybeEnrichBuddyTLVForICQ6Viewer(true, userInfo, buddySess, bos)
}

// tlvUserInfoHasICQFlag reports whether the UserFlags TLV (0x01) of `info`
// has the ICQ bit (0x0040) set.
func tlvUserInfoHasICQFlag(info wire.TLVUserInfo) bool {
	flags, ok := info.Uint16BE(wire.OServiceUserInfoUserFlags)
	return ok && flags&wire.OServiceUserFlagICQ == wire.OServiceUserFlagICQ
}

// sessionHasICQFlag reports whether `sess` is an ICQ account at the session
// level, or has the ICQ user flag set on at least one of its instances.
func sessionHasICQFlag(sess *state.Session) bool {
	if sess == nil {
		return false
	}
	if sess.ICQAccount() {
		return true
	}
	for _, inst := range sess.Instances() {
		if inst == nil {
			continue
		}
		if inst.UserInfoBitmask()&wire.OServiceUserFlagICQ == wire.OServiceUserFlagICQ {
			return true
		}
	}
	return false
}

func (s buddyNotifier) BroadcastBuddyDeparted(ctx context.Context, screenName state.IdentScreenName) error {
	users, err := s.relationshipFetcher.AllRelationships(ctx, screenName, nil)
	if err != nil {
		return err
	}

	var recipients []state.IdentScreenName
	for _, user := range users {
		if user.YouBlock || user.BlocksYou || !user.IsOnTheirList {
			continue
		}
		recipients = append(recipients, s.canonicalBuddyIdent(ctx, user.User))
	}

	departName := screenName.String()
	if s.sessionRetriever != nil {
		if sess := s.sessionRetriever.RetrieveSession(screenName); sess != nil {
			departName = sess.BuddyWireScreenName()
		}
	}

	s.messageRelayer.RelayToScreenNames(ctx, recipients, wire.SNACMessage{
		Frame: wire.SNACFrame{
			FoodGroup: wire.Buddy,
			SubGroup:  wire.BuddyDeparted,
			RequestID: wire.ReqIDFromServer,
		},
		Body: wire.SNAC_0x03_0x0C_BuddyDeparted{
			TLVUserInfo: wire.TLVUserInfo{
				// don't include the TLV block, otherwise the AIM client fails
				// to process the block event
				ScreenName:   departName,
				WarningLevel: 0,
				TLVBlock: wire.TLVBlock{
					TLVList: wire.TLVList{
						// this TLV needs to be set in order for departure
						// events to work in ICQ
						wire.NewTLVBE(wire.OServiceUserInfoUserFlags, uint16(0)),
					},
				},
			},
		},
	})

	return nil
}

// BroadcastVisibility sends you and related users arrival/departure
// notifications that reflect your buddy list and privacy preferences.
//
// Behavior:
//   - Sends you arrival notifications for users on your buddy list that I do
//     not block.
//   - Sends arrival notifications to users that you block who have you on
//     their buddy lists.
//   - Sends you departure notifications for users on your buddy list that you
//     block  (if doSendDepartures is true).
//   - Sends departure notifications to users that you block who have you on
//     their buddy lists (if doSendDepartures is true).
//   - Don't send notifications for any user that blocks you.
//
// This method is called when your visibility settings change, ensuring that
// all relevant users are notified of your arrival or departure status.
func (s buddyNotifier) BroadcastVisibility(
	ctx context.Context,
	you *state.SessionInstance,
	filter []state.IdentScreenName,
	doSendDepartures bool,
) error {

	relationships, err := s.relationshipFetcher.AllRelationships(ctx, you.IdentScreenName(), filter)
	if err != nil {
		return fmt.Errorf("retrieving relationships: %w", err)
	}

	yourTLVInfo := you.Session().BuddyTLVUserInfo()

	for _, relationship := range relationships {
		if relationship.BlocksYou {
			continue // they block you, don't send them notifications
		}

		theirSess := s.retrieveLiveBuddySession(ctx, relationship.User)
		if theirSess == nil {
			continue // they are offline
		}

		if !relationship.YouBlock {
			if relationship.IsOnTheirList {
				// tell them you're online
				s.unicastBuddyArrived(ctx, yourTLVInfo, theirSess.IdentScreenName())
			}
			if relationship.IsOnYourList {
				theirInfo := theirSess.BuddyTLVUserInfo()
				// tell you they're online
				s.unicastBuddyArrived(ctx, theirInfo, you.IdentScreenName())
			}
		} else if relationship.YouBlock && doSendDepartures {
			if relationship.IsOnTheirList {
				// tell them you're offline
				s.unicastBuddyDeparted(ctx, you.Session(), theirSess.IdentScreenName())
			}
			if relationship.IsOnYourList {
				// tell you they're offline
				s.unicastBuddyDeparted(ctx, theirSess, you.IdentScreenName())
			}
		}
	}

	return nil
}

func (s buddyNotifier) unicastBuddyDeparted(ctx context.Context, from *state.Session, to state.IdentScreenName) {
	s.messageRelayer.RelayToScreenName(ctx, to, wire.SNACMessage{
		Frame: wire.SNACFrame{
			FoodGroup: wire.Buddy,
			SubGroup:  wire.BuddyDeparted,
			RequestID: wire.ReqIDFromServer,
		},
		Body: wire.SNAC_0x03_0x0C_BuddyDeparted{
			TLVUserInfo: wire.TLVUserInfo{
				// don't include the TLV block, otherwise the AIM client fails
				// to process the block event
				ScreenName:   from.BuddyWireScreenName(),
				WarningLevel: from.Warning(),
			},
		},
	})
}

// unicastBuddyArrived sends the latest user info to a particular user.
// While updates are sent via the wire.BuddyArrived SNAC, the message is not
// only used to indicate the user coming online. It can also notify changes to
// buddy icons, warning levels, invisibility status, etc.
func (s buddyNotifier) unicastBuddyArrived(ctx context.Context, userInfo wire.TLVUserInfo, to state.IdentScreenName) {
	if userInfo.IsInvisible() {
		return
	}
	about := state.NormalizeICQUINBuddyKey(state.NewIdentScreenName(userInfo.ScreenName))
	ui := s.buddyArrivedTLVForRecipient(ctx, to, about, userInfo)
	s.messageRelayer.RelayToScreenName(ctx, to, wire.SNACMessage{
		Frame: wire.SNACFrame{
			FoodGroup: wire.Buddy,
			SubGroup:  wire.BuddyArrived,
			RequestID: wire.ReqIDFromServer,
		},
		Body: wire.SNAC_0x03_0x0B_BuddyArrived{
			TLVUserInfo: ui,
		},
	})
}
