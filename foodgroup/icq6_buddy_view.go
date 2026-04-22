package foodgroup

import (
	"encoding/binary"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"time"

	"github.com/mk6i/open-oscar-server/state"
	"github.com/mk6i/open-oscar-server/wire"
)

func cloneTLVUserInfo(ui wire.TLVUserInfo) wire.TLVUserInfo {
	out := ui
	out.TLVList = append(wire.TLVList(nil), ui.TLVList...)
	return out
}

func capsFromOscarTLV(raw []byte) [][16]byte {
	var out [][16]byte
	for i := 0; i+16 <= len(raw); i += 16 {
		var c [16]byte
		copy(c[:], raw[i:i+16])
		out = append(out, c)
	}
	return out
}

func capsToBytes(caps [][16]byte) []byte {
	b := make([]byte, 0, len(caps)*16)
	for _, c := range caps {
		b = append(b, c[:]...)
	}
	return b
}

func sortCaps(caps [][16]byte) {
	slices.SortFunc(caps, func(a, b [16]byte) int {
		for i := 0; i < 16; i++ {
			if a[i] != b[i] {
				return int(a[i]) - int(b[i])
			}
		}
		return 0
	})
}

func icqBuddyDCFromSession(sess *state.Session) (wire.ICQDCInfo, bool) {
	if sess == nil {
		return wire.ICQDCInfo{}, false
	}
	for _, inst := range sess.Instances() {
		if inst == nil {
			continue
		}
		ap := inst.RemoteAddr()
		if ap == nil {
			continue
		}
		addr := ap.Addr()
		if addr.Is4In6() {
			addr = addr.Unmap()
		}
		if !addr.Is4() {
			continue
		}
		ip4 := addr.As4()
		dc := wire.ICQDCInfo{
			IP:   binary.BigEndian.Uint32(ip4[:]),
			Port: uint32(ap.Port()),
		}
		return dc, true
	}
	return wire.ICQDCInfo{}, false
}

func enrichICQ6BuddyDC(dc wire.ICQDCInfo, nowUnix uint32) wire.ICQDCInfo {
	if dc.IP == 0 {
		return dc
	}
	if dc.DCType == 0 {
		// OSCAR ICQ DC type list: 0x01 = HTTPS/firewall proxy, 0x04 = normal DC.
		// Defaulting to 0x01 makes ICQ6 treat the peer as proxy-mode and keep the
		// "flower" offline-style indicator even when IP:port are reachable.
		dc.DCType = wire.ICQDCTypeNormal
	}
	if dc.ProtoVersion == 0 {
		dc.ProtoVersion = 10
	}
	if dc.LastUpdateTime == 0 {
		dc.LastUpdateTime = nowUnix
	}
	if dc.LastExtInfoUpdateTime == 0 {
		dc.LastExtInfoUpdateTime = nowUnix
	}
	if dc.LastExtStatusUpdateTime == 0 {
		dc.LastExtStatusUpdateTime = nowUnix
	}
	return dc
}

func parseLiteralIPv4HostPort(bosHostPort string) (ipU32 uint32, port uint32, ok bool) {
	host, portStr, err := net.SplitHostPort(bosHostPort)
	if err != nil {
		return 0, 0, false
	}
	port = 5190
	if portStr != "" {
		if p, err := strconv.ParseUint(portStr, 10, 16); err == nil {
			port = uint32(p)
		}
	}
	ipAddr, err := netip.ParseAddr(host)
	if err != nil || !ipAddr.Is4() {
		return 0, 0, false
	}
	ip4 := ipAddr.As4()
	return binary.BigEndian.Uint32(ip4[:]), port, true
}

func icqDCPeerAddressNeedsSubstitute(ipU32 uint32) bool {
	if ipU32 == 0 {
		return true
	}
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], ipU32)
	ip := net.IP(b[:])
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
}

// maybeEnrichBuddyTLVForICQ6Viewer returns a copy of base with TLV tweaks that
// ICQ 2003 / ICQ 6-generation clients expect for buddy flowers. Other clients
// receive base unchanged (same struct; TLV slice is not copied).
//
// strict selects the ICQ-specific TLV shaping path (OscarCaps, UserFlags2, DC,
// ICQ external IP 0x0A). BuddyArrived uses strict=true for ICQ accounts or
// ICQ-flagged recipient instances.
//
// bosAdvertisedHostPort is the BOS plaintext advert (often :5190 FLAP); used
// when the peer DC address is private or missing so TLV 0x0C can fall back to
// a server-published IPv4 literal.
func maybeEnrichBuddyTLVForICQ6Viewer(strict bool, base wire.TLVUserInfo, buddySess *state.Session, bosAdvertisedHostPort string) wire.TLVUserInfo {
	if !strict {
		return base
	}
	flags, ok := base.Uint16BE(wire.OServiceUserInfoUserFlags)
	if !ok || flags&wire.OServiceUserFlagICQ != wire.OServiceUserFlagICQ {
		return base
	}
	out := cloneTLVUserInfo(base)
	now := uint32(time.Now().Unix())

	// TLV 0x1F: ICQ 6 reads extended user flags on buddy blocks; zero is common.
	if !out.HasTag(wire.OServiceUserInfoUserFlags2) {
		out.Append(wire.NewTLVBE(wire.OServiceUserInfoUserFlags2, uint32(0)))
	}

	// TLV 0x0D: ICQ 6 classifies peers partly from OscarCaps; Locate omitCaps
	// strips CapSupportICQ — add it back only in this viewer-specific copy.
	var caps [][16]byte
	if raw, has := out.Bytes(wire.OServiceUserInfoOscarCaps); has && len(raw) > 0 {
		caps = capsFromOscarTLV(raw)
	}
	support := [16]byte(wire.CapSupportICQ)
	if !slices.ContainsFunc(caps, func(c [16]byte) bool { return c == support }) {
		caps = append(caps, support)
	}
	if len(caps) > 0 {
		sortCaps(caps)
		capsTLV := wire.NewTLVBE(wire.OServiceUserInfoOscarCaps, capsToBytes(caps))
		if out.HasTag(wire.OServiceUserInfoOscarCaps) {
			out.Replace(capsTLV)
		} else {
			out.Append(capsTLV)
		}
	}

	// Direct-connect refresh needs a live buddy session with an IPv4 from the
	// FLAP remote address. If lookup failed, OscarCaps + UserFlags2 above still
	// help flower coloring for ICQ viewers receiving enriched BuddyArrived blocks.
	if buddySess == nil {
		return out
	}

	dc, haveDC := icqBuddyDCFromSession(buddySess)

	// The session-derived DC info comes from the buddy's BOS TCP source IP/port.
	// It is only useful when the IP is publicly reachable. For RFC1918 / loopback
	// / link-local IPs the address is unreachable for any remote ICQ peer, AND
	// the source port is almost certainly an ephemeral port (not the buddy's
	// configured DC listener), so leaking it into TLV 0x0C makes ICQ6 mark the
	// peer as DC-unreachable ("white flower"). In that case throw it away and
	// fall through to the BOS advert below when it is an IPv4 literal.
	if haveDC && (dc.IP == 0 || icqDCPeerAddressNeedsSubstitute(dc.IP)) {
		dc = wire.ICQDCInfo{}
		haveDC = false
	}

	if !haveDC && bosAdvertisedHostPort != "" {
		if ipU32, p, ok := parseLiteralIPv4HostPort(bosAdvertisedHostPort); ok {
			dc = wire.ICQDCInfo{IP: ipU32, Port: p}
			haveDC = true
		}
	}
	if haveDC && dc.IP != 0 {
		dc = enrichICQ6BuddyDC(dc, now)
		if out.HasTag(wire.OServiceUserInfoICQDC) {
			out.Replace(wire.NewTLVBE(wire.OServiceUserInfoICQDC, dc))
		} else {
			out.Append(wire.NewTLVBE(wire.OServiceUserInfoICQDC, dc))
		}
		extTLV := wire.NewTLVBE(wire.OServiceUserInfoICQExternalIP, dc.IP)
		if out.HasTag(wire.OServiceUserInfoICQExternalIP) {
			out.Replace(extTLV)
		} else {
			out.Append(extTLV)
		}
	}

	return out
}
