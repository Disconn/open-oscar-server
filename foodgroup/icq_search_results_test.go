package foodgroup

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/mk6i/open-oscar-server/state"
	"github.com/mk6i/open-oscar-server/wire"
)

func TestSendICQUserSearchResults_zeroHitsUsesOKAndLastResult(t *testing.T) {
	rel := newMockMessageRelayer(t)
	rel.EXPECT().RelayToSelf(mock.Anything, matchSession(state.NewIdentScreenName("11111111")), mock.Anything).
		Run(func(_ context.Context, _ *state.SessionInstance, msg wire.SNACMessage) {
			reply, ok := msg.Body.(wire.SNAC_0x15_0x02_DBReply)
			require.True(t, ok)
			got, ok := reply.Bytes(wire.ICQTLVTagsMetadata)
			require.True(t, ok)

			last := wire.ICQ_0x07DA_0x01AE_DBQueryMetaReplyLastUserFound{
				ICQMetadata: wire.ICQMetadata{
					UIN:     11111111,
					ReqType: wire.ICQDBQueryMetaReply,
					Seq:     9,
				},
				ReqSubType: wire.ICQDBQueryMetaReplyLastUserFound,
				Success:    wire.ICQStatusCodeOK,
				Details:    wire.ICQUserSearchRecord{},
			}
			last.LastResult()
			expMsg := wire.ICQMessageReplyEnvelope{Message: last}

			var want bytes.Buffer
			require.NoError(t, wire.MarshalBE(expMsg, &want))
			require.Equal(t, want.Bytes(), got)
		}).Once()

	s := ICQService{
		messageRelayer: rel,
	}
	require.NoError(t, s.sendICQUserSearchResults(context.Background(), newTestInstance("11111111", sessOptUIN(11111111)), 9, nil, "test_zero_hits"))
}
