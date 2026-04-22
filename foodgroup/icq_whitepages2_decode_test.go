package foodgroup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mk6i/open-oscar-server/wire"
)

func TestWhitePages2FieldString_plainUTF8Value(t *testing.T) {
	// No ICQString wrapper and no UTF-16 nul pairs — previously decoded as absent.
	list := wire.TLVList{wire.TLV{Tag: wire.ICQTLVTagsNickname, Value: []byte("plainnick")}}
	s, ok := whitePages2FieldString(&list, wire.ICQTLVTagsNickname)
	require.True(t, ok)
	assert.Equal(t, "plainnick", s)
}

func TestWhitePages2FieldStringWP_directoryNicknameTag(t *testing.T) {
	list := wire.TLVList{wire.TLV{Tag: wire.ICQDirTLVTagsNickname, Value: []byte("fromdir")}}
	s, ok := whitePages2FieldStringWP(&list, wire.ICQTLVTagsNickname, wire.ICQDirTLVTagsNickname)
	require.True(t, ok)
	assert.Equal(t, "fromdir", s)
}

func TestDecodeWhitePages2UINFromTLVs_asciiTLV0032(t *testing.T) {
	list := wire.TLVList{wire.TLV{Tag: 0x0032, Value: []byte("123456789")}}
	u, ok := decodeWhitePages2UINFromTLVs(&list)
	require.True(t, ok)
	assert.Equal(t, uint32(123456789), u)
}

func TestDecodeWhitePages2UINFromTLVs_dirUINASCII(t *testing.T) {
	list := wire.TLVList{wire.TLV{Tag: wire.ICQDirTLVTagsUINASCII, Value: []byte("987654321")}}
	u, ok := decodeWhitePages2UINFromTLVs(&list)
	require.True(t, ok)
	assert.Equal(t, uint32(987654321), u)
}
