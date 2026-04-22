package foodgroup

import (
	"encoding/binary"
	"net"
	"net/netip"
	"testing"

	"github.com/mk6i/open-oscar-server/state"
	"github.com/mk6i/open-oscar-server/wire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_maybeEnrichBuddyTLVForICQ6Viewer_addsCapSupportICQAndDC(t *testing.T) {
	sess := state.NewSession()
	sess.SetIdentScreenName(state.NewIdentScreenName("100001"))
	sess.SetDisplayScreenName("100001")
	inst := sess.AddInstance()
	inst.SetUserInfoFlag(wire.OServiceUserFlagICQ)
	ap, err := netip.ParseAddrPort("10.0.0.2:5190")
	require.NoError(t, err)
	inst.SetRemoteAddr(&ap)
	sess.SetICQBOSAdvertisedHostPort("203.0.113.1:5190")

	base := wire.TLVUserInfo{
		ScreenName: "100001",
		TLVBlock: wire.TLVBlock{
			TLVList: wire.TLVList{
				wire.NewTLVBE(wire.OServiceUserInfoUserFlags, wire.OServiceUserFlagOSCARFree|wire.OServiceUserFlagICQ),
				wire.NewTLVBE(wire.OServiceUserInfoStatus, uint32(0)),
			},
		},
	}
	out := maybeEnrichBuddyTLVForICQ6Viewer(true, base, sess, sess.ICQBOSAdvertisedHostPort())
	raw, ok := out.Bytes(wire.OServiceUserInfoOscarCaps)
	require.True(t, ok)
	require.GreaterOrEqual(t, len(raw), 16)
	want := [16]byte(wire.CapSupportICQ)
	found := false
	for _, c := range capsFromOscarTLV(raw) {
		if c == want {
			found = true
			break
		}
	}
	assert.True(t, found)
	assert.True(t, out.HasTag(wire.OServiceUserInfoICQDC))
	rawDC, ok := out.Bytes(wire.OServiceUserInfoICQDC)
	require.True(t, ok)
	require.GreaterOrEqual(t, len(rawDC), 9)
	assert.Equal(t, wire.ICQDCTypeNormal, rawDC[8], "TLV 0x0C DC type must be DC_NORMAL (0x04), not DC_HTTPS (0x01)")
	assert.True(t, out.HasTag(wire.OServiceUserInfoICQExternalIP))
	rawExt, ok := out.Bytes(wire.OServiceUserInfoICQExternalIP)
	require.True(t, ok)
	require.GreaterOrEqual(t, len(rawExt), 4)
	assert.Equal(t, binary.BigEndian.Uint32(rawDC[0:4]), binary.BigEndian.Uint32(rawExt[0:4]))
}

// Test_maybeEnrichBuddyTLVForICQ6Viewer_privatePeerIPSubstituted verifies the
// presence-shaping contract for ICQ6: a buddy whose BOS source IP is RFC1918
// (e.g. behind NAT or on the same LAN as the server) MUST NOT have that
// private IP/port leaked into TLV 0x0C. The source port is almost always
// ephemeral (not the buddy's actual DC listener), so passing it through marks
// the buddy as DC-unreachable in ICQ6 ("white flower"). Enrichment falls back
// to the BOS-advertised host:port so ICQ6 can use that IPv4 literal in TLV 0x0C.
func Test_maybeEnrichBuddyTLVForICQ6Viewer_privatePeerIPSubstituted(t *testing.T) {
	sess := state.NewSession()
	sess.SetIdentScreenName(state.NewIdentScreenName("100002"))
	sess.SetDisplayScreenName("100002")
	inst := sess.AddInstance()
	inst.SetUserInfoFlag(wire.OServiceUserFlagICQ)
	ap, err := netip.ParseAddrPort("192.168.1.50:40001")
	require.NoError(t, err)
	inst.SetRemoteAddr(&ap)

	base := wire.TLVUserInfo{
		ScreenName: "100002",
		TLVBlock: wire.TLVBlock{
			TLVList: wire.TLVList{
				wire.NewTLVBE(wire.OServiceUserInfoUserFlags, wire.OServiceUserFlagOSCARFree|wire.OServiceUserFlagICQ),
			},
		},
	}
	out := maybeEnrichBuddyTLVForICQ6Viewer(true, base, sess, "203.0.113.9:5190")
	raw, ok := out.Bytes(wire.OServiceUserInfoICQDC)
	require.True(t, ok)
	require.GreaterOrEqual(t, len(raw), 8)
	wantIP := binary.BigEndian.Uint32(net.IPv4(203, 0, 113, 9).To4())
	gotIP := binary.BigEndian.Uint32(raw[0:4])
	gotPort := binary.BigEndian.Uint32(raw[4:8])
	assert.Equal(t, wantIP, gotIP, "private peer IP must be replaced with BOS advert IP")
	assert.Equal(t, uint32(5190), gotPort, "ephemeral peer port must be replaced with BOS advert port")
}

func Test_maybeEnrichBuddyTLVForICQ6Viewer_ipv6OnlyPeerFallsBackToBOSAdvertised(t *testing.T) {
	sess := state.NewSession()
	sess.SetIdentScreenName(state.NewIdentScreenName("100004"))
	sess.SetDisplayScreenName("100004")
	inst := sess.AddInstance()
	inst.SetUserInfoFlag(wire.OServiceUserFlagICQ)
	ap, err := netip.ParseAddrPort("[2001:db8::1]:5190")
	require.NoError(t, err)
	inst.SetRemoteAddr(&ap)
	sess.SetICQBOSAdvertisedHostPort("198.51.100.7:5190")

	base := wire.TLVUserInfo{
		ScreenName: "100004",
		TLVBlock: wire.TLVBlock{
			TLVList: wire.TLVList{
				wire.NewTLVBE(wire.OServiceUserInfoUserFlags, wire.OServiceUserFlagOSCARFree|wire.OServiceUserFlagICQ),
			},
		},
	}
	out := maybeEnrichBuddyTLVForICQ6Viewer(true, base, sess, sess.ICQBOSAdvertisedHostPort())
	require.True(t, out.HasTag(wire.OServiceUserInfoICQDC))
	raw, ok := out.Bytes(wire.OServiceUserInfoICQDC)
	require.True(t, ok)
	wantIP := binary.BigEndian.Uint32(net.IPv4(198, 51, 100, 7).To4())
	assert.Equal(t, wantIP, binary.BigEndian.Uint32(raw[0:4]))
	assert.Equal(t, uint32(5190), binary.BigEndian.Uint32(raw[4:8]))
	assert.True(t, out.HasTag(wire.OServiceUserInfoICQExternalIP))
}

func Test_maybeEnrichBuddyTLVForICQ6Viewer_strictNilBuddySessionStillAddsCaps(t *testing.T) {
	base := wire.TLVUserInfo{
		ScreenName: "12345678",
		TLVBlock: wire.TLVBlock{
			TLVList: wire.TLVList{
				wire.NewTLVBE(wire.OServiceUserInfoUserFlags, wire.OServiceUserFlagOSCARFree|wire.OServiceUserFlagICQ),
				wire.NewTLVBE(wire.OServiceUserInfoStatus, uint32(0)),
			},
		},
	}
	out := maybeEnrichBuddyTLVForICQ6Viewer(true, base, nil, "")
	raw, ok := out.Bytes(wire.OServiceUserInfoOscarCaps)
	require.True(t, ok)
	require.GreaterOrEqual(t, len(raw), 16)
	want := [16]byte(wire.CapSupportICQ)
	found := false
	for _, c := range capsFromOscarTLV(raw) {
		if c == want {
			found = true
			break
		}
	}
	assert.True(t, found)
	assert.True(t, out.HasTag(wire.OServiceUserInfoUserFlags2))
}

// Test_maybeEnrichBuddyTLVForICQ6Viewer_replacesZeroDCFromUserInfo reproduces
// the live BroadcastBuddyArrived path: state.Session.userInfo() always emits
// a zero ICQDCInfo TLV (see state/session.go), and the buddy's RemoteAddr is a
// private IPv4. Enrichment MUST overwrite that zero TLV using the BOS-advert
// IPv4 literal so ICQ6 does not keep 0.0.0.0:0.
func Test_maybeEnrichBuddyTLVForICQ6Viewer_replacesZeroDCFromUserInfo(t *testing.T) {
	sess := state.NewSession()
	sess.SetIdentScreenName(state.NewIdentScreenName("365199535"))
	sess.SetDisplayScreenName("365199535")
	inst := sess.AddInstance()
	inst.SetUserInfoFlag(wire.OServiceUserFlagICQ | wire.OServiceUserFlagOSCARFree | wire.OServiceUserFlagUnconfirmed)
	ap, err := netip.ParseAddrPort("10.245.1.2:64605")
	require.NoError(t, err)
	inst.SetRemoteAddr(&ap)
	sess.SetICQBOSAdvertisedHostPort("10.245.1.2:5190")

	// Mirror exactly what state.Session.userInfo() builds for an ICQ session:
	// the ICQDC TLV is present with ALL zeros (wire.ICQDCInfo{}). Enrichment
	// must Replace that, not skip it.
	base := wire.TLVUserInfo{
		ScreenName: "365199535",
		TLVBlock: wire.TLVBlock{
			TLVList: wire.TLVList{
				wire.NewTLVBE(wire.OServiceUserInfoUserFlags, uint16(0x0051)),
				wire.NewTLVBE(wire.OServiceUserInfoStatus, uint32(0x10000000)),
				wire.NewTLVBE(wire.OServiceUserInfoICQExternalIP, uint32(0x0AF50102)),
				wire.NewTLVBE(wire.OServiceUserInfoICQDC, wire.ICQDCInfo{}),
				wire.NewTLVBE(wire.OServiceUserInfoOscarCaps, []byte{}),
			},
		},
	}
	out := maybeEnrichBuddyTLVForICQ6Viewer(true, base, sess, sess.ICQBOSAdvertisedHostPort())

	require.True(t, out.HasTag(wire.OServiceUserInfoICQDC), "DC TLV must still be present")
	rawDC, ok := out.Bytes(wire.OServiceUserInfoICQDC)
	require.True(t, ok)
	require.Equal(t, 37, len(rawDC), "DC TLV must be 37 bytes (ICQDCInfo struct size)")

	wantIP := binary.BigEndian.Uint32(net.IPv4(10, 245, 1, 2).To4())
	gotIP := binary.BigEndian.Uint32(rawDC[0:4])
	assert.Equal(t, wantIP, gotIP, "DC IP must be replaced with BOS advert IP, not left as 0")
	assert.Equal(t, uint32(5190), binary.BigEndian.Uint32(rawDC[4:8]),
		"DC port must be BOS advert port (5190), not the original 0")
	assert.Equal(t, wire.ICQDCTypeNormal, rawDC[8], "DCType must be DC_NORMAL")
}

func Test_maybeEnrichBuddyTLVForICQ6Viewer_nonICQViewerUnchanged(t *testing.T) {
	sess := state.NewSession()
	sess.SetIdentScreenName(state.NewIdentScreenName("100001"))
	sess.SetDisplayScreenName("100001")
	inst := sess.AddInstance()
	inst.SetUserInfoFlag(wire.OServiceUserFlagICQ)

	base := wire.TLVUserInfo{
		ScreenName: "100001",
		TLVBlock: wire.TLVBlock{
			TLVList: wire.TLVList{
				wire.NewTLVBE(wire.OServiceUserInfoUserFlags, wire.OServiceUserFlagOSCARFree|wire.OServiceUserFlagICQ),
			},
		},
	}
	out := maybeEnrichBuddyTLVForICQ6Viewer(false, base, sess, "")
	assert.Equal(t, base.TLVList, out.TLVList)
}
