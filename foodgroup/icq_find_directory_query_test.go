package foodgroup

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"io"
	"log/slog"
	"testing"

	"github.com/mk6i/open-oscar-server/state"
	"github.com/mk6i/open-oscar-server/wire"
)

// icq6DirSearchSampleHex is defined in icq_directory_extract_test.go (same package).

// Directory UIN search with embedded 0x0002 (mitschnitt suchen 2.pcapng client → server).
const icq6DirUINSearchEmbed02Hex = "2F0005B900028000000000000006000100020002000004E400000002000200000001000D00320009313233343536373839"

// Directory nickname search (embedded 0x0006 + UTF-16 nickname TLV).
const icq6DirNickSearchHex = "05B9000680000000000000780006006D00610078"

// decodeDirectoryQueryReplyMetaSubtype reads the ICQ meta ReqSubType from the
// wire.ICQTLVTagsMetadata TLV (MarshalBE envelope + LE ICQ payload).
func decodeDirectoryQueryReplyMetaSubtype(t *testing.T, msg wire.SNACMessage) uint16 {
	t.Helper()
	reply, ok := msg.Body.(wire.SNAC_0x15_0x02_DBReply)
	require.True(t, ok)
	md, ok := reply.Bytes(wire.ICQTLVTagsMetadata)
	require.True(t, ok, "metadata TLV missing")
	require.GreaterOrEqual(t, len(md), 2+10)
	innerLen := int(binary.LittleEndian.Uint16(md[0:2]))
	require.GreaterOrEqual(t, len(md), 2+innerLen)
	inner := md[2 : 2+innerLen]
	require.GreaterOrEqual(t, len(inner), 10)
	return binary.LittleEndian.Uint16(inner[8:10])
}

// decodeDirectoryQueryEmbeddedSubtype reads the embedded 0x05B9 SNAC subtype
// from the Data payload that follows the ICQ meta header (0x0FB4 directory replies).
func decodeDirectoryQueryEmbeddedSubtype(t *testing.T, msg wire.SNACMessage) uint16 {
	t.Helper()
	reply, ok := msg.Body.(wire.SNAC_0x15_0x02_DBReply)
	require.True(t, ok)
	md, ok := reply.Bytes(wire.ICQTLVTagsMetadata)
	require.True(t, ok, "metadata TLV missing")
	innerLen := int(binary.LittleEndian.Uint16(md[0:2]))
	inner := md[2 : 2+innerLen]
	// ICQ meta header: UIN(4) reqType(2) seq(2) subtype(2) status(1) = 11 bytes.
	require.GreaterOrEqual(t, len(inner), 11+4, "embedded SNAC header missing")
	embSNAC := inner[11:]
	require.Equal(t, uint16(0x05B9), binary.BigEndian.Uint16(embSNAC[0:2]), "expected embedded family 0x05B9")
	return binary.BigEndian.Uint16(embSNAC[2:4])
}

// TestFindByDirectoryQuery_singleUINSearch_noHit verifies that a 0x0FA0
// user search with a UIN that does not resolve sends a single 0x0FB4 frame
// with embedded 0x0003 terminator (tmp3.txt / AOL reference; not 0x0002).
func TestFindByDirectoryQuery_singleUINSearch_noHit(t *testing.T) {
	raw, err := hex.DecodeString(icq6DirUINSearchEmbed02Hex)
	require.NoError(t, err)

	finder := newMockICQUserFinder(t)
	finder.EXPECT().FindByUIN(mock.Anything, uint32(123456789)).Return(state.User{}, state.ErrNoUser)

	var metaSubs []uint16
	var inner []uint16
	rel := newMockMessageRelayer(t)
	rel.EXPECT().RelayToSelf(mock.Anything, matchSession(state.NewIdentScreenName("11111111")), mock.Anything).
		Run(func(_ context.Context, _ *state.SessionInstance, msg wire.SNACMessage) {
			metaSubs = append(metaSubs, decodeDirectoryQueryReplyMetaSubtype(t, msg))
			inner = append(inner, decodeDirectoryQueryEmbeddedSubtype(t, msg))
		}).Once()

	s := NewICQService(rel, finder, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	inst := newTestInstance("11111111", sessOptUIN(11111111))
	require.NoError(t, s.FindByDirectoryQuery(context.Background(), inst, raw, 7))
	assert.Equal(t, []uint16{wire.ICQDBQueryMetaReplyDirectoryResponse}, metaSubs)
	assert.Equal(t, []uint16{0x0003}, inner)
}

// TestFindByDirectoryQuery_singleUINSearch_oneHit verifies a single hit sends
// two 0x0FB4 frames: embedded 0x0009 (row) then 0x0003 (terminator), tmp3.txt.
func TestFindByDirectoryQuery_singleUINSearch_oneHit(t *testing.T) {
	raw, err := hex.DecodeString(icq6DirUINSearchEmbed02Hex)
	require.NoError(t, err)

	hit := state.User{
		IdentScreenName: state.NewIdentScreenName("123456789"),
		ICQBasicInfo: state.ICQBasicInfo{
			Nickname:  "target",
			FirstName: "T",
			LastName:  "User",
		},
	}
	finder := newMockICQUserFinder(t)
	finder.EXPECT().FindByUIN(mock.Anything, uint32(123456789)).Return(hit, nil)

	var metaSubs []uint16
	var inner []uint16
	rel := newMockMessageRelayer(t)
	rel.EXPECT().RelayToSelf(mock.Anything, matchSession(state.NewIdentScreenName("11111111")), mock.Anything).
		Run(func(_ context.Context, _ *state.SessionInstance, msg wire.SNACMessage) {
			metaSubs = append(metaSubs, decodeDirectoryQueryReplyMetaSubtype(t, msg))
			inner = append(inner, decodeDirectoryQueryEmbeddedSubtype(t, msg))
		}).Times(2)

	s := NewICQService(rel, finder, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	inst := newTestInstance("11111111", sessOptUIN(11111111))
	require.NoError(t, s.FindByDirectoryQuery(context.Background(), inst, raw, 3))
	assert.Equal(t, []uint16{
		wire.ICQDBQueryMetaReplyDirectoryResponse,
		wire.ICQDBQueryMetaReplyDirectoryResponse,
	}, metaSubs)
	assert.Equal(t, []uint16{0x0009, 0x0003}, inner)
}

// TestFindByDirectoryQuery_nickSearch_twoHits verifies name/nick search uses three
// 0x0FB4 frames: embedded 0x0009 per row then 0x0003 terminator.
func TestFindByDirectoryQuery_nickSearch_twoHits(t *testing.T) {
	raw, err := hex.DecodeString(icq6DirNickSearchHex)
	require.NoError(t, err)

	u1 := state.User{
		IdentScreenName: state.NewIdentScreenName("200001"),
		ICQBasicInfo:    state.ICQBasicInfo{Nickname: "max", FirstName: "A"},
	}
	u2 := state.User{
		IdentScreenName: state.NewIdentScreenName("200002"),
		ICQBasicInfo:    state.ICQBasicInfo{Nickname: "max", FirstName: "B"},
	}
	finder := newMockICQUserFinder(t)
	// ICQ6 prefix rewrite: bare nick "max" -> "max%" on the pattern finder.
	finder.EXPECT().FindByICQNamePattern(mock.Anything, "", "", "max%").Return([]state.User{u1, u2}, nil)

	var metaSubs []uint16
	var inner []uint16
	rel := newMockMessageRelayer(t)
	rel.EXPECT().RelayToSelf(mock.Anything, matchSession(state.NewIdentScreenName("11111111")), mock.Anything).
		Run(func(_ context.Context, _ *state.SessionInstance, msg wire.SNACMessage) {
			metaSubs = append(metaSubs, decodeDirectoryQueryReplyMetaSubtype(t, msg))
			inner = append(inner, decodeDirectoryQueryEmbeddedSubtype(t, msg))
		}).Times(3)

	s := NewICQService(rel, finder, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	inst := newTestInstance("11111111", sessOptUIN(11111111))
	require.NoError(t, s.FindByDirectoryQuery(context.Background(), inst, raw, 1))
	// Nick TLV path uses embedded 0x0006 (icq6DirNickSearchHex) → 0x0FB4 / 0x0009+0x0003
	// per mitschnitt suchen 3.pcapng.
	assert.Equal(t, []uint16{
		wire.ICQDBQueryMetaReplyDirectoryResponse,
		wire.ICQDBQueryMetaReplyDirectoryResponse,
		wire.ICQDBQueryMetaReplyDirectoryResponse,
	}, metaSubs)
	assert.Equal(t, []uint16{0x0009, 0x0009, 0x0003}, inner)
}

// TestFindByDirectoryQuery_bulkUINResolve verifies that a single 0x0006
// directory query with multiple ASCII UIN TLVs (the buddy-list "fill in
// details" path) keeps the custom 0x0FB4 envelope. This path is not a
// user-visible search and ICQ6 parses the response internally.
func TestFindByDirectoryQuery_bulkUINResolve(t *testing.T) {
	// Embedded SNAC 0x05B9 / 0x0006, then three repeated buddy blocks each
	// containing TLV 0x0032 (ASCII UIN, length 9): 100000001, 200000002, 300000003.
	raw, err := hex.DecodeString(
		"05B900068000000000000000" +
			"00320009313030303030303031" +
			"00320009323030303030303032" +
			"00320009333030303030303033",
	)
	require.NoError(t, err)

	mk := func(uin uint32, nick string) state.User {
		return state.User{
			IdentScreenName: state.NewIdentScreenName(fmt.Sprintf("%d", uin)),
			ICQBasicInfo:    state.ICQBasicInfo{Nickname: nick, FirstName: nick},
		}
	}
	finder := newMockICQUserFinder(t)
	finder.EXPECT().FindByUIN(mock.Anything, uint32(100000001)).Return(mk(100000001, "alice"), nil)
	finder.EXPECT().FindByUIN(mock.Anything, uint32(200000002)).Return(mk(200000002, "bob"), nil)
	finder.EXPECT().FindByUIN(mock.Anything, uint32(300000003)).Return(mk(300000003, "carol"), nil)

	var outer []uint16
	var inner []uint16
	rel := newMockMessageRelayer(t)
	rel.EXPECT().RelayToSelf(mock.Anything, matchSession(state.NewIdentScreenName("11111111")), mock.Anything).
		Run(func(_ context.Context, _ *state.SessionInstance, msg wire.SNACMessage) {
			outer = append(outer, decodeDirectoryQueryReplyMetaSubtype(t, msg))
			inner = append(inner, decodeDirectoryQueryEmbeddedSubtype(t, msg))
		}).Times(4)

	s := NewICQService(rel, finder, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	inst := newTestInstance("11111111", sessOptUIN(11111111))
	require.NoError(t, s.FindByDirectoryQuery(context.Background(), inst, raw, 11))
	// Three row frames (embedded 0x0009) + one terminator (embedded 0x0003),
	// all carried on outer meta 0x0FB4.
	assert.Equal(t, []uint16{
		wire.ICQDBQueryMetaReplyDirectoryResponse,
		wire.ICQDBQueryMetaReplyDirectoryResponse,
		wire.ICQDBQueryMetaReplyDirectoryResponse,
		wire.ICQDBQueryMetaReplyDirectoryResponse,
	}, outer)
	assert.Equal(t, []uint16{0x0009, 0x0009, 0x0009, 0x0003}, inner)
}

func TestFindByDirectoryQuery_selfCheck_unchanged(t *testing.T) {
	// Self-check: embedded 0x0002, only signing UIN in criteria — not effectiveSearch.
	raw, err := hex.DecodeString("05B90002800000000000003200093131313131313131")
	require.NoError(t, err)

	selfUser := state.User{
		IdentScreenName: state.NewIdentScreenName("11111111"),
		ICQBasicInfo:    state.ICQBasicInfo{Nickname: "self"},
	}
	finder := newMockICQUserFinder(t)
	finder.EXPECT().FindByUIN(mock.Anything, uint32(11111111)).Return(selfUser, nil)
	rel := newMockMessageRelayer(t)
	rel.EXPECT().RelayToSelf(mock.Anything, matchSession(state.NewIdentScreenName("11111111")), mock.Anything).Times(2)

	s := NewICQService(rel, finder, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	inst := newTestInstance("11111111", sessOptUIN(11111111))
	require.NoError(t, s.FindByDirectoryQuery(context.Background(), inst, raw, 9))
}

func TestAckDirectoryUpdate(t *testing.T) {
	var metaSubs []uint16
	var inner []uint16
	rel := newMockMessageRelayer(t)
	rel.EXPECT().RelayToSelf(mock.Anything, matchSession(state.NewIdentScreenName("11111111")), mock.Anything).
		Run(func(_ context.Context, _ *state.SessionInstance, msg wire.SNACMessage) {
			metaSubs = append(metaSubs, decodeDirectoryQueryReplyMetaSubtype(t, msg))
			inner = append(inner, decodeDirectoryQueryEmbeddedSubtype(t, msg))
		}).Once()

	s := NewICQService(rel, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	inst := newTestInstance("11111111", sessOptUIN(11111111))
	require.NoError(t, s.AckDirectoryUpdate(context.Background(), inst, 42))
	assert.Equal(t, []uint16{wire.ICQDBQueryMetaReplyDirectoryResponse}, metaSubs)
	assert.Equal(t, []uint16{0x0003}, inner)
}
