package foodgroup

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mk6i/open-oscar-server/wire"
)

// Real ICQ6 directory UIN-search frame from OSCAR logs (LE TLV for UIN 123456789).
const icq6DirSearchSampleHex = "370005B900068000000000000006000100020002000004E4000000020001001900640008000000000000000000320009313233343536373839"

func TestExtractDirQueryCriteria_ICQ6SampleUIN(t *testing.T) {
	raw, err := hex.DecodeString(icq6DirSearchSampleHex)
	assert.NoError(t, err)
	q := extractDirQueryCriteria(alignDirectoryQueryBody(raw))
	assert.Equal(t, uint32(123456789), q.UIN)
}

// Directory nickname TLV 0x0078 as UTF-16 BE (ICQ6 directory search).
const icq6DirNickUTF16Hex = "05B9000680000000000000780006006D00610078"

func TestWhitePages2FieldString_UTF16LENickname(t *testing.T) {
	// ICQ6-style bare UTF-16LE "Max" + NUL pair in a nickname TLV.
	maxUTF16 := []byte{0x4D, 0x00, 0x61, 0x00, 0x78, 0x00, 0x00, 0x00}
	list := wire.TLVList{wire.NewTLVLE(wire.ICQTLVTagsNickname, maxUTF16)}
	got, ok := whitePages2FieldString(&list, wire.ICQTLVTagsNickname)
	require.True(t, ok)
	assert.Equal(t, "Max", got)
}

func TestWhitePages2FieldString_UTF16BENickname(t *testing.T) {
	maxUTF16BE := []byte{0x00, 0x4D, 0x00, 0x61, 0x00, 0x78, 0x00, 0x00}
	list := wire.TLVList{wire.NewTLVLE(wire.ICQTLVTagsNickname, maxUTF16BE)}
	got, ok := whitePages2FieldString(&list, wire.ICQTLVTagsNickname)
	require.True(t, ok)
	assert.Equal(t, "Max", got)
}

func TestExtractDirQueryCriteria_ICQ6UTF16Nickname(t *testing.T) {
	raw, err := hex.DecodeString(icq6DirNickUTF16Hex)
	assert.NoError(t, err)
	q := extractDirQueryCriteria(raw)
	assert.Equal(t, "max", q.NickName)
}

func TestDirQueryLooksLikeDirectorySearch(t *testing.T) {
	assert.False(t, dirQueryLooksLikeDirectorySearch(dirQueryCriteria{}, 100))
	assert.False(t, dirQueryLooksLikeDirectorySearch(dirQueryCriteria{UIN: 100}, 100))
	assert.True(t, dirQueryLooksLikeDirectorySearch(dirQueryCriteria{UIN: 200}, 100))
	assert.True(t, dirQueryLooksLikeDirectorySearch(dirQueryCriteria{NickName: "x"}, 100))
}

// Two ASCII UIN TLVs (0x0032): contact then self — extract used to keep only the last.
// Second UIN must be nine ASCII digits (100000001); one 0x30 was missing in the log-derived hex.
const icq6DirTwoUINsHex = "05B900068000000000000006000100020002000004E4000000020002001900640008000000000000000000320009313233343536373839001900640008000000000000000000320009313030303030303031"

func TestPickDirectorySearchUIN_prefersFirstNonSelf(t *testing.T) {
	raw, err := hex.DecodeString(icq6DirTwoUINsHex)
	assert.NoError(t, err)
	selfUIN := uint32(365199535)
	picked := pickDirectorySearchUIN(raw, selfUIN)
	assert.Equal(t, uint32(123456789), picked)
	assert.GreaterOrEqual(t, len(collectDirQueryUINCandidates(raw)), 2)
}

func TestDecodeWhitePages2InterestsSearch(t *testing.T) {
	// TLV value: LE category 10, LE length including NUL, asciz keywords.
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint16(10))
	kw := "Music,Hiking\x00"
	_ = binary.Write(&buf, binary.LittleEndian, uint16(len(kw)))
	buf.WriteString(kw)
	list := wire.TLVList{wire.NewTLVLE(wire.ICQTLVTagsInterestsNode, buf.Bytes())}
	code, kws, ok := decodeWhitePages2InterestsSearch(&list)
	require.True(t, ok)
	assert.Equal(t, uint16(10), code)
	assert.Equal(t, []string{"Music", "Hiking"}, kws)
}

func TestDecodeWhitePages2UINFromTLVs(t *testing.T) {
	t.Run("four_byte_le_uint32", func(t *testing.T) {
		list := wire.TLVList{wire.NewTLVLE(wire.ICQTLVTagsUIN, uint32(123456789))}
		u, ok := decodeWhitePages2UINFromTLVs(&list)
		require.True(t, ok)
		assert.Equal(t, uint32(123456789), u)
	})
	t.Run("len_prefixed_ascii", func(t *testing.T) {
		list := wire.TLVList{
			wire.NewTLVLE(wire.ICQTLVTagsUIN, struct {
				Val string `oscar:"len_prefix=uint16,nullterm"`
			}{Val: "365199535"}),
		}
		u, ok := decodeWhitePages2UINFromTLVs(&list)
		require.True(t, ok)
		assert.Equal(t, uint32(365199535), u)
	})
}

func TestAlignDirectoryQueryBody(t *testing.T) {
	t.Run("le_length_prefix", func(t *testing.T) {
		raw, err := hex.DecodeString(icq6DirUINSearchEmbed02Hex)
		require.NoError(t, err)
		b := alignDirectoryQueryBody(raw)
		require.GreaterOrEqual(t, len(b), 4)
		assert.Equal(t, uint16(0x05B9), binary.BigEndian.Uint16(b[0:2]))
		assert.Equal(t, uint16(0x0002), binary.BigEndian.Uint16(b[2:4]))
	})
	t.Run("single_byte_junk_prefix", func(t *testing.T) {
		inner, err := hex.DecodeString("05B900028000000000000006000100020002000004E400000002000200000001000D00320009313233343536373839")
		require.NoError(t, err)
		raw := append([]byte{0x01}, inner...)
		b := alignDirectoryQueryBody(raw)
		assert.Equal(t, inner, b)
	})
	t.Run("aligns_any_05b9_subtype", func(t *testing.T) {
		// Previously only 0x0002/0x0006 were accepted in the scan loop; align to any 0x05B9.
		inner, err := hex.DecodeString("05B9000A8000000000000000")
		require.NoError(t, err)
		raw := append([]byte{0xAA, 0xBB, 0xCC}, inner...)
		b := alignDirectoryQueryBody(raw)
		assert.Equal(t, inner, b)
		assert.Equal(t, uint16(0x000A), binary.BigEndian.Uint16(b[2:4]))
	})
}
