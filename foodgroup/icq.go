package foodgroup

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/mk6i/open-oscar-server/internal/icqctx"
	"github.com/mk6i/open-oscar-server/state"
	"github.com/mk6i/open-oscar-server/wire"
)

const icqSearchMaxResults = 40

type icqNamePatternFinder interface {
	FindByICQNamePattern(ctx context.Context, firstPat, lastPat, nickPat string) ([]state.User, error)
}

type icqEmailPatternFinder interface {
	FindByICQEmailPattern(ctx context.Context, pat string) ([]state.User, error)
}

// icqWhitePages2PatternFinder is implemented by *state.SQLiteUserStore for TLV
// WhitePages2 searches that combine name fields with home city/state (0x0190/0x019A).
type icqWhitePages2PatternFinder interface {
	FindByICQNameAndHomeLocationPattern(ctx context.Context, firstPat, lastPat, nickPat, cityPat, statePat string) ([]state.User, error)
}

// icqWildcardToSQLLike maps client * and ? to SQL LIKE % and _, escaping % _ \.
func icqWildcardToSQLLike(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	s = strings.ReplaceAll(s, "*", "%")
	s = strings.ReplaceAll(s, "?", "_")
	return s
}

func splitCommaKeywords(s string) []string {
	parts := strings.Split(s, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func mergeUsersByIdent(existing []state.User, add []state.User) []state.User {
	seen := make(map[string]bool, len(existing)+len(add))
	out := make([]state.User, 0, len(existing)+len(add))
	for _, u := range existing {
		k := u.IdentScreenName.String()
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, u)
	}
	for _, u := range add {
		k := u.IdentScreenName.String()
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, u)
	}
	return out
}

func icqContainsFold(hay, needle string) bool {
	if needle == "" {
		return true
	}
	if hay == "" {
		return false
	}
	return strings.Contains(strings.ToLower(hay), strings.ToLower(needle))
}

func filterUsersForWhitePagesPlain(users []state.User, inBody wire.ICQ_0x07D0_0x0533_DBQueryMetaReqSearchWhitePages, now func() time.Time) []state.User {
	if len(users) == 0 {
		return users
	}
	out := make([]state.User, 0, len(users))
	for _, user := range users {
		if inBody.MinAge > 0 && inBody.MaxAge > 0 {
			age := uint16(user.Age(now))
			if age < inBody.MinAge || age > inBody.MaxAge {
				continue
			}
		}
		if inBody.Gender > 0 && inBody.Gender < 16 {
			if uint8(user.ICQMoreInfo.Gender) != inBody.Gender {
				continue
			}
		}
		if inBody.SpeakingLang > 0 && inBody.SpeakingLang < 127 {
			if user.ICQMoreInfo.Lang1 != inBody.SpeakingLang &&
				user.ICQMoreInfo.Lang2 != inBody.SpeakingLang &&
				user.ICQMoreInfo.Lang3 != inBody.SpeakingLang {
				continue
			}
		}
		if inBody.CountryCode > 0 && inBody.CountryCode < 20000 {
			if user.ICQBasicInfo.CountryCode != inBody.CountryCode {
				continue
			}
		}
		if !icqContainsFold(user.ICQBasicInfo.City, inBody.City) {
			continue
		}
		if !icqContainsFold(user.ICQBasicInfo.State, inBody.State) {
			continue
		}
		if !icqContainsFold(user.ICQWorkInfo.Company, inBody.Company) {
			continue
		}
		if !icqContainsFold(user.ICQWorkInfo.Position, inBody.Position) {
			continue
		}
		if inBody.OccupationCode > 0 && inBody.OccupationCode < 60000 {
			if user.ICQWorkInfo.OccupationCode != inBody.OccupationCode {
				continue
			}
		}
		out = append(out, user)
	}
	return out
}

var errICQBadRequest = errors.New("bad ICQ request")

// NewICQService creates an instance of ICQService.
func NewICQService(
	messageRelayer MessageRelayer,
	finder ICQUserFinder,
	userUpdater ICQUserUpdater,
	logger *slog.Logger,
	sessionRetriever SessionRetriever,
	offlineMessageManager OfflineMessageManager,
	icbmSender func(ctx context.Context, instance *state.SessionInstance, inFrame wire.SNACFrame, inBody wire.SNAC_0x04_0x06_ICBMChannelMsgToHost) (*wire.SNACMessage, error),
) ICQService {
	return ICQService{
		messageRelayer:        messageRelayer,
		userFinder:            finder,
		userUpdater:           userUpdater,
		logger:                logger,
		sessionRetriever:      sessionRetriever,
		offlineMessageManager: offlineMessageManager,
		icbmSender:            icbmSender,
		timeNow:               time.Now,
	}
}

// ICQService provides functionality for the ICQ food group.
type ICQService struct {
	userFinder            ICQUserFinder
	logger                *slog.Logger
	messageRelayer        MessageRelayer
	sessionRetriever      SessionRetriever
	userUpdater           ICQUserUpdater
	timeNow               func() time.Time
	offlineMessageManager OfflineMessageManager
	icbmSender            func(ctx context.Context, instance *state.SessionInstance, inFrame wire.SNACFrame, inBody wire.SNAC_0x04_0x06_ICBMChannelMsgToHost) (*wire.SNACMessage, error)
}

func (s ICQService) DeleteMsgReq(ctx context.Context, instance *state.SessionInstance, seq uint16) error {
	if err := s.offlineMessageManager.DeleteMessages(ctx, instance.IdentScreenName()); err != nil {
		return fmt.Errorf("deleting messages: %w", err)
	}
	return nil
}

// MetaTerminalAck sends SNAC(0x15,0x03) with a single ICQ meta reply (0x07DA) ending in
// 0x01AE + LastResult footer and no user Details. ICQ 5/6 expect this shape instead of
// OSCAR SNAC(0x15,0x01) when a meta request is accepted or completed with no payload.
func (s ICQService) MetaTerminalAck(ctx context.Context, instance *state.SessionInstance, seq uint16, success uint8) error {
	resp := wire.ICQ_0x07DA_0x01AE_DBQueryMetaReplyLastUserFound{
		ICQMetadata: wire.ICQMetadata{
			UIN:     instance.UIN(),
			ReqType: wire.ICQDBQueryMetaReply,
			Seq:     seq,
		},
		ReqSubType: wire.ICQDBQueryMetaReplyLastUserFound,
		Success:    success,
	}
	resp.LastResult()
	return s.reply(ctx, instance, wire.ICQMessageReplyEnvelope{Message: resp})
}

// sendICQUserSearchResults sends SNAC(0x15,0x03) meta replies for an ICQ search.
// Per OSCAR ICQ search sequences (e.g. iserverd / sobek docs, mirrored on
// web.archive.org): intermediate hits use 0x07DA/0x01A4 (META_USER_FOUND), and
// the terminal packet uses 0x07DA/0x01AE (META_LAST_USER_FOUND) with the
// LastResult footer. ICQ 6 follows that pattern; sending only 0x01AE frames
// with FoundUsersLeft confused some builds and produced a generic “search
// failed” dialog despite hits.
func (s ICQService) sendICQUserSearchResults(ctx context.Context, instance *state.SessionInstance, seq uint16, users []state.User, logLabel string) error {
	rid, ridOK := icqctx.SNACRequestID(ctx)
	if s.logger != nil {
		s.logger.DebugContext(ctx, "ICQ search outgoing sequence",
			"search_kind", logLabel,
			"hits", len(users),
			"meta_seq", seq,
			"snac_request_id", rid,
			"snac_request_id_set", ridOK,
		)
	}

	if len(users) == 0 {
		// ICQ6 treats ICQStatusCodeFail here as a hard search failure (“could not be
		// performed”). A completed search with zero matches uses OK + LastResult, like
		// AOL reference traffic for directory meta (Success 0x0A).
		resp := wire.ICQ_0x07DA_0x01AE_DBQueryMetaReplyLastUserFound{
			ICQMetadata: wire.ICQMetadata{
				UIN:     instance.UIN(),
				ReqType: wire.ICQDBQueryMetaReply,
				Seq:     seq,
			},
			ReqSubType: wire.ICQDBQueryMetaReplyLastUserFound,
			Success:    wire.ICQStatusCodeOK,
			Details:    wire.ICQUserSearchRecord{},
		}
		resp.LastResult()
		return s.reply(ctx, instance, wire.ICQMessageReplyEnvelope{Message: resp})
	}

	if len(users) > icqSearchMaxResults {
		users = users[:icqSearchMaxResults]
	}

	meta := wire.ICQMetadata{
		UIN:     instance.UIN(),
		ReqType: wire.ICQDBQueryMetaReply,
		Seq:     seq,
	}

	if len(users) == 1 {
		resp := wire.ICQ_0x07DA_0x01AE_DBQueryMetaReplyLastUserFound{
			ICQMetadata: meta,
			ReqSubType:  wire.ICQDBQueryMetaReplyLastUserFound,
			Success:     wire.ICQStatusCodeOK,
			Details:     s.createResult(users[0]),
		}
		resp.LastResult()
		return s.reply(ctx, instance, wire.ICQMessageReplyEnvelope{Message: resp})
	}

	for i := 0; i < len(users)-1; i++ {
		resp := wire.ICQ_0x07DA_0x01AE_DBQueryMetaReplyLastUserFound{
			ICQMetadata: meta,
			ReqSubType:  wire.ICQDBQueryMetaReplyUserFound,
			Success:     wire.ICQStatusCodeOK,
			Details:     s.createResult(users[i]),
		}
		if err := s.reply(ctx, instance, wire.ICQMessageReplyEnvelope{Message: resp}); err != nil {
			return err
		}
	}
	last := wire.ICQ_0x07DA_0x01AE_DBQueryMetaReplyLastUserFound{
		ICQMetadata: meta,
		ReqSubType:  wire.ICQDBQueryMetaReplyLastUserFound,
		Success:     wire.ICQStatusCodeOK,
		Details:     s.createResult(users[len(users)-1]),
	}
	last.LastResult()
	return s.reply(ctx, instance, wire.ICQMessageReplyEnvelope{Message: last})
}

func (s ICQService) whitePagesPlainCollectUsers(ctx context.Context, inBody wire.ICQ_0x07D0_0x0533_DBQueryMetaReqSearchWhitePages) ([]state.User, error) {
	var all []state.User
	var firstErr error

	hasBasic := inBody.FirstName != "" || inBody.LastName != "" || inBody.Nickname != "" || inBody.Email != ""
	wildcardWP := icqctx.WhitePagesPlainWildcard(ctx)
	if hasBasic {
		if inBody.Email != "" {
			if wildcardWP {
				if ef, ok := s.userFinder.(icqEmailPatternFinder); ok {
					pat := icqWildcardToSQLLike(inBody.Email)
					if pat != "" {
						uu, err := ef.FindByICQEmailPattern(ctx, pat)
						if err != nil && firstErr == nil {
							firstErr = err
						} else if err == nil {
							all = mergeUsersByIdent(all, uu)
						}
					}
				} else {
					u, err := s.userFinder.FindByICQEmail(ctx, inBody.Email)
					if err == nil {
						all = append(all, u)
					} else if !errors.Is(err, state.ErrNoUser) && firstErr == nil {
						firstErr = err
					}
				}
			} else {
				u, err := s.userFinder.FindByICQEmail(ctx, inBody.Email)
				if err == nil {
					all = append(all, u)
				} else if !errors.Is(err, state.ErrNoUser) && firstErr == nil {
					firstErr = err
				}
			}
		}
		if len(all) == 0 && (inBody.Nickname != "" || inBody.FirstName != "" || inBody.LastName != "") {
			if wildcardWP {
				if pf, ok := s.userFinder.(icqNamePatternFinder); ok {
					fp := icqWildcardToSQLLike(inBody.FirstName)
					lp := icqWildcardToSQLLike(inBody.LastName)
					np := icqWildcardToSQLLike(inBody.Nickname)
					if fp != "" || lp != "" || np != "" {
						uu, err := pf.FindByICQNamePattern(ctx, fp, lp, np)
						if err != nil && firstErr == nil {
							firstErr = err
						} else if err == nil {
							all = mergeUsersByIdent(all, uu)
						}
					}
				} else {
					uu, err := s.userFinder.FindByICQName(ctx, inBody.FirstName, inBody.LastName, inBody.Nickname)
					if err != nil && firstErr == nil {
						firstErr = err
					} else {
						all = mergeUsersByIdent(all, uu)
					}
				}
			} else {
				uu, err := s.userFinder.FindByICQName(ctx, inBody.FirstName, inBody.LastName, inBody.Nickname)
				if err != nil && firstErr == nil {
					firstErr = err
				} else {
					all = mergeUsersByIdent(all, uu)
				}
			}
		}
	}

	if inBody.InterestsCode > 0 && inBody.InterestsCode < 60000 {
		kws := splitCommaKeywords(inBody.InterestsKeyword)
		if len(kws) > 0 {
			uu, err := s.userFinder.FindByICQInterests(ctx, inBody.InterestsCode, kws)
			if err != nil && firstErr == nil {
				firstErr = err
			} else if err == nil {
				all = mergeUsersByIdent(all, uu)
			}
		}
	}
	if strings.TrimSpace(inBody.InterestsKeyword) != "" && inBody.InterestsCode == 0 {
		uu, err := s.userFinder.FindByICQKeyword(ctx, strings.TrimSpace(inBody.InterestsKeyword))
		if err != nil && firstErr == nil {
			firstErr = err
		} else if err == nil {
			all = mergeUsersByIdent(all, uu)
		}
	}
	if kw := strings.TrimSpace(inBody.PastKeywords); kw != "" {
		uu, err := s.userFinder.FindByICQKeyword(ctx, kw)
		if err == nil {
			all = mergeUsersByIdent(all, uu)
		}
	}
	if kw := strings.TrimSpace(inBody.HomePageKeywords); kw != "" {
		uu, err := s.userFinder.FindByICQKeyword(ctx, kw)
		if err == nil {
			all = mergeUsersByIdent(all, uu)
		}
	}
	return all, firstErr
}

func (s ICQService) FindByICQName(ctx context.Context, instance *state.SessionInstance, inBody wire.ICQ_0x07D0_0x0515_DBQueryMetaReqSearchByDetails, seq uint16) error {
	res, err := s.userFinder.FindByICQName(ctx, inBody.FirstName, inBody.LastName, inBody.NickName)
	if err != nil {
		s.logger.Error("FindByICQName failed", "err", err.Error())
		resp := wire.ICQ_0x07DA_0x01AE_DBQueryMetaReplyLastUserFound{
			ICQMetadata: wire.ICQMetadata{
				UIN:     instance.UIN(),
				ReqType: wire.ICQDBQueryMetaReply,
				Seq:     seq,
			},
			Success:    wire.ICQStatusCodeErr,
			ReqSubType: wire.ICQDBQueryMetaReplyLastUserFound,
		}
		resp.LastResult()
		return s.reply(ctx, instance, wire.ICQMessageReplyEnvelope{Message: resp})
	}
	return s.sendICQUserSearchResults(ctx, instance, seq, res, "search_by_details")
}

// FindByICQDetailsWildcard handles SNAC(0x15,0x02) meta subtype 0x053D (same layout as 0x0515).
func (s ICQService) FindByICQDetailsWildcard(ctx context.Context, instance *state.SessionInstance, inBody wire.ICQ_0x07D0_0x0515_DBQueryMetaReqSearchByDetails, seq uint16) error {
	fp := icqWildcardToSQLLike(inBody.FirstName)
	lp := icqWildcardToSQLLike(inBody.LastName)
	np := icqWildcardToSQLLike(inBody.NickName)
	var res []state.User
	var err error
	if pf, ok := s.userFinder.(icqNamePatternFinder); ok && (fp != "" || lp != "" || np != "") {
		res, err = pf.FindByICQNamePattern(ctx, fp, lp, np)
	} else {
		res, err = s.userFinder.FindByICQName(ctx, inBody.FirstName, inBody.LastName, inBody.NickName)
	}
	if err != nil {
		s.logger.Error("FindByICQDetailsWildcard failed", "err", err.Error())
		resp := wire.ICQ_0x07DA_0x01AE_DBQueryMetaReplyLastUserFound{
			ICQMetadata: wire.ICQMetadata{
				UIN:     instance.UIN(),
				ReqType: wire.ICQDBQueryMetaReply,
				Seq:     seq,
			},
			Success:    wire.ICQStatusCodeErr,
			ReqSubType: wire.ICQDBQueryMetaReplyLastUserFound,
		}
		resp.LastResult()
		return s.reply(ctx, instance, wire.ICQMessageReplyEnvelope{Message: resp})
	}
	return s.sendICQUserSearchResults(ctx, instance, seq, res, "search_by_details_wildcard")
}

// FindByICQEmailWildcard handles meta subtype 0x0547 (same layout as 0x0529).
func (s ICQService) FindByICQEmailWildcard(ctx context.Context, instance *state.SessionInstance, inBody wire.ICQ_0x07D0_0x0529_DBQueryMetaReqSearchByEmail, seq uint16) error {
	pat := icqWildcardToSQLLike(inBody.Email)
	var res []state.User
	var err error
	if pf, ok := s.userFinder.(icqEmailPatternFinder); ok && pat != "" {
		res, err = pf.FindByICQEmailPattern(ctx, pat)
	} else if u, e := s.userFinder.FindByICQEmail(ctx, inBody.Email); e == nil {
		res = []state.User{u}
	} else if errors.Is(e, state.ErrNoUser) {
		res = nil
	} else {
		err = e
	}
	if err != nil {
		s.logger.Error("FindByICQEmailWildcard failed", "err", err.Error())
		resp := wire.ICQ_0x07DA_0x01AE_DBQueryMetaReplyLastUserFound{
			ICQMetadata: wire.ICQMetadata{
				UIN:     instance.UIN(),
				ReqType: wire.ICQDBQueryMetaReply,
				Seq:     seq,
			},
			Success:    wire.ICQStatusCodeErr,
			ReqSubType: wire.ICQDBQueryMetaReplyLastUserFound,
		}
		resp.LastResult()
		return s.reply(ctx, instance, wire.ICQMessageReplyEnvelope{Message: resp})
	}
	return s.sendICQUserSearchResults(ctx, instance, seq, res, "search_by_email_wildcard")
}

func (s ICQService) FindByICQEmail(ctx context.Context, instance *state.SessionInstance, inBody wire.ICQ_0x07D0_0x0529_DBQueryMetaReqSearchByEmail, seq uint16) error {
	resp := wire.ICQ_0x07DA_0x01AE_DBQueryMetaReplyLastUserFound{
		ICQMetadata: wire.ICQMetadata{
			UIN:     instance.UIN(),
			ReqType: wire.ICQDBQueryMetaReply,
			Seq:     seq,
		},
		ReqSubType: wire.ICQDBQueryMetaReplyLastUserFound,
		Success:    wire.ICQStatusCodeOK,
	}
	resp.LastResult()

	res, err := s.userFinder.FindByICQEmail(ctx, inBody.Email)

	switch {
	case errors.Is(err, state.ErrNoUser):
		resp.Success = wire.ICQStatusCodeFail
	case err != nil:
		s.logger.Error("FindByICQEmail failed", "err", err.Error())
		resp.Success = wire.ICQStatusCodeErr
	default:
		resp.Success = wire.ICQStatusCodeOK
		resp.Details = s.createResult(res)
	}

	return s.reply(ctx, instance, wire.ICQMessageReplyEnvelope{
		Message: resp,
	})
}

func (s ICQService) FindByEmail3(ctx context.Context, instance *state.SessionInstance, inBody wire.ICQ_0x07D0_0x0573_DBQueryMetaReqSearchByEmail3, seq uint16) error {
	b, hasEmail := inBody.Bytes(wire.ICQTLVTagsEmail)
	if !hasEmail {
		return errors.New("unable to get email from request")
	}

	email := wire.ICQEmail{}
	if err := wire.UnmarshalLE(&email, bytes.NewReader(b)); err != nil {
		return fmt.Errorf("unmarshal email: %w", err)
	}

	resp := wire.ICQ_0x07DA_0x01AE_DBQueryMetaReplyLastUserFound{
		ICQMetadata: wire.ICQMetadata{
			UIN:     instance.UIN(),
			ReqType: wire.ICQDBQueryMetaReply,
			Seq:     seq,
		},
		ReqSubType: wire.ICQDBQueryMetaReplyLastUserFound,
		Success:    wire.ICQStatusCodeOK,
	}
	resp.LastResult()

	res, err := s.userFinder.FindByICQEmail(ctx, email.Email)

	switch {
	case errors.Is(err, state.ErrNoUser):
		resp.Success = wire.ICQStatusCodeFail
	case err != nil:
		s.logger.Error("FindByICQEmail failed", "err", err.Error())
		resp.Success = wire.ICQStatusCodeErr
	default:
		resp.Success = wire.ICQStatusCodeOK
		resp.Details = s.createResult(res)
	}

	return s.reply(ctx, instance, wire.ICQMessageReplyEnvelope{
		Message: resp,
	})
}

// FindByICQInterests handles plain whitepages SNAC meta subtype 0x0533 (not
// "interests only" — it uses name/email/location/interest criteria per OSCAR).
func (s ICQService) FindByICQInterests(ctx context.Context, instance *state.SessionInstance, inBody wire.ICQ_0x07D0_0x0533_DBQueryMetaReqSearchWhitePages, seq uint16) error {
	res, err := s.whitePagesPlainCollectUsers(ctx, inBody)
	if err != nil {
		s.logger.Error("whitepages plain search failed", "err", err.Error())
		resp := wire.ICQ_0x07DA_0x01AE_DBQueryMetaReplyLastUserFound{
			ICQMetadata: wire.ICQMetadata{
				UIN:     instance.UIN(),
				ReqType: wire.ICQDBQueryMetaReply,
				Seq:     seq,
			},
			Success:    wire.ICQStatusCodeErr,
			ReqSubType: wire.ICQDBQueryMetaReplyLastUserFound,
		}
		resp.LastResult()
		return s.reply(ctx, instance, wire.ICQMessageReplyEnvelope{Message: resp})
	}
	filtered := filterUsersForWhitePagesPlain(res, inBody, s.timeNow)
	return s.sendICQUserSearchResults(ctx, instance, seq, filtered, "search_whitepages_plain")
}

// decodeWhitePages2UINFromTLVs reads TLV 0x0136 (ICQTLVTagsUIN) from a
// ICQDBQueryMetaReqSearchWhitePages2 (0x055F) request. ICQ6 may send a 4-byte
// UIN as LE or BE, or a LE length–prefixed ASCII decimal string + NUL (same
// outer layout as ICQString). Without this path, the client only sends UIN
// (no nick/first/last) and the server replied with “no criteria” → generic
// search failure in the directory UI.
func decodeWhitePages2UINFromTLVs(list *wire.TLVList) (uint32, bool) {
	for _, tlv := range *list {
		if tlv.Tag == wire.ICQDirTLVTagsUINASCII || tlv.Tag == 0x0032 {
			v := tlv.Value
			if len(v) >= 5 && len(v) <= 12 && isAllDigits(v) {
				if n, err := strconv.ParseUint(string(v), 10, 32); err == nil && n > 0 {
					return uint32(n), true
				}
			}
			continue
		}
		if tlv.Tag != wire.ICQTLVTagsUIN {
			continue
		}
		v := tlv.Value
		if len(v) >= 3 {
			expected := int(binary.LittleEndian.Uint16(v[0:2]))
			if expected == len(v)-2 && len(v) >= 3 && v[len(v)-1] == 0 {
				dec := strings.TrimSpace(string(v[2 : len(v)-1]))
				if len(dec) >= 5 && len(dec) <= 12 && isAllDigits([]byte(dec)) {
					if n, err := strconv.ParseUint(dec, 10, 32); err == nil && n > 0 {
						return uint32(n), true
					}
				}
			}
		}
		if len(v) == 4 {
			if le := binary.LittleEndian.Uint32(v); le != 0 {
				return le, true
			}
			if be := binary.BigEndian.Uint32(v); be != 0 {
				return be, true
			}
		}
	}
	return 0, false
}

// decodeWhitePages2InterestsSearch reads TLV 0x01EA (ICQTLVTagsInterestsNode) on
// WhitePages2: LE category + LE keyword-blob length + keyword string (often
// comma-separated), per iserverd 0x055F documentation.
func decodeWhitePages2InterestsSearch(list *wire.TLVList) (code uint16, keywords []string, ok bool) {
	raw, got := list.Bytes(wire.ICQTLVTagsInterestsNode)
	if !got || len(raw) < 4 {
		return 0, nil, false
	}
	code = binary.LittleEndian.Uint16(raw[0:2])
	l := int(binary.LittleEndian.Uint16(raw[2:4]))
	if l < 0 || 4+l > len(raw) {
		return 0, nil, false
	}
	s := string(raw[4 : 4+l])
	s = strings.TrimRight(s, "\x00")
	s = strings.TrimSpace(s)
	if strings.TrimSpace(s) == "" {
		return 0, nil, false
	}
	kws := splitCommaKeywords(s)
	if len(kws) == 0 {
		return 0, nil, false
	}
	return code, kws, true
}

// icqTLVUTF16Payload decodes UTF-16 LE or BE text (nul-terminated code units)
// when ICQString does not apply. ICQ6 uses LE in some builds and BE in others.
func icqTLVUTF16Payload(raw []byte) (string, bool) {
	if len(raw) < 2 || len(raw)%2 != 0 {
		return "", false
	}
	// UTF-16 BMP Latin uses zero high or low bytes — pure UTF-8 ASCII has none.
	if bytes.IndexByte(raw, 0) < 0 {
		return "", false
	}
	decode := func(order binary.ByteOrder) string {
		u16s := make([]uint16, len(raw)/2)
		for i := 0; i < len(raw); i += 2 {
			u16s[i/2] = order.Uint16(raw[i : i+2])
		}
		for len(u16s) > 0 && u16s[len(u16s)-1] == 0 {
			u16s = u16s[:len(u16s)-1]
		}
		if len(u16s) == 0 {
			return ""
		}
		return string(utf16.Decode(u16s))
	}
	sLE := strings.TrimSpace(decode(binary.LittleEndian))
	sBE := strings.TrimSpace(decode(binary.BigEndian))
	asciiRunes := func(s string) int {
		if s == "" || !utf8.ValidString(s) {
			return -1
		}
		n := 0
		for _, r := range s {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
				n++
			case r == ' ', r == '_', r == '.', r == '@', r == '-', r == ',':
				n++
			}
		}
		return n
	}
	score := func(s string) int {
		if s == "" || !utf8.ValidString(s) {
			return -1
		}
		n := 0
		for _, r := range s {
			if r >= 32 && r < 0xFE00 {
				n++
			}
		}
		return n
	}
	al, ab := asciiRunes(sLE), asciiRunes(sBE)
	sl, sb := score(sLE), score(sBE)
	switch {
	case ab > al:
		return sBE, true
	case al > ab:
		return sLE, true
	case sb > sl && sb > 0:
		return sBE, true
	case sl > 0:
		return sLE, true
	case sb > 0:
		return sBE, true
	default:
		return "", false
	}
}

// whitePages2FieldString reads a WhitePages2 string TLV: LE-len UTF-8 + NUL
// (ICQString) or raw UTF-16 (LE/BE) when ICQString does not match.
func whitePages2FieldString(list *wire.TLVList, tag uint16) (string, bool) {
	if s, ok := list.ICQString(tag); ok {
		return s, true
	}
	raw, ok := list.Bytes(tag)
	if !ok {
		return "", false
	}
	if s, ok := icqTLVUTF16Payload(raw); ok {
		return s, true
	}
	// ICQ6 often sends UTF-8 or UTF-16 where ICQString's length check fails; still treat as present.
	s := strings.TrimSpace(strings.TrimRight(string(raw), "\x00"))
	if s != "" && utf8.ValidString(s) {
		return s, true
	}
	if len(raw) == 0 {
		return "", true
	}
	return "", false
}

// whitePages2FieldStringWP tries the modern WhitePages2 TLV tag, then the older
// directory-style tag (same on-wire numbers as ICQDirTLVTags*) some ICQ6 builds use.
func whitePages2FieldStringWP(list *wire.TLVList, modernTag, dirTag uint16) (string, bool) {
	s, ok := whitePages2FieldString(list, modernTag)
	if ok {
		return s, ok
	}
	return whitePages2FieldString(list, dirTag)
}

func filterWhitePages2ByHomeCityState(users []state.User, city, stateAbbr string) []state.User {
	cityNeedle := strings.TrimSpace(strings.TrimRight(strings.TrimRight(city, "%"), "."))
	stateNeedle := strings.TrimSpace(strings.TrimRight(strings.TrimRight(stateAbbr, "%"), "."))
	if cityNeedle == "" && stateNeedle == "" {
		return users
	}
	out := make([]state.User, 0, len(users))
	for _, u := range users {
		if cityNeedle != "" && !icqContainsFold(u.ICQBasicInfo.City, cityNeedle) {
			continue
		}
		if stateNeedle != "" && !icqContainsFold(u.ICQBasicInfo.State, stateNeedle) {
			continue
		}
		out = append(out, u)
	}
	return out
}

func (s ICQService) FindByWhitePages2(ctx context.Context, instance *state.SessionInstance, inBody wire.ICQ_0x07D0_0x055F_DBQueryMetaReqSearchWhitePages2, seq uint16) error {

	users, err := func() ([]state.User, error) {
		kwStr, kwTLV := whitePages2FieldString(&inBody.TLVList, wire.ICQTLVTagsWhitepagesSearchKeywords)
		kwStr = strings.TrimSpace(kwStr)
		if kwTLV && kwStr != "" {
			res, err := s.userFinder.FindByICQKeyword(ctx, kwStr)
			if err != nil {
				return nil, fmt.Errorf("FindByICQKeyword failed: %w", err)
			}
			return res, nil
		}

		bNick, nickTLV := whitePages2FieldStringWP(&inBody.TLVList, wire.ICQTLVTagsNickname, wire.ICQDirTLVTagsNickname)
		bFirst, firstTLV := whitePages2FieldStringWP(&inBody.TLVList, wire.ICQTLVTagsFirstName, wire.ICQDirTLVTagsFirstName)
		bLast, lastTLV := whitePages2FieldStringWP(&inBody.TLVList, wire.ICQTLVTagsLastName, wire.ICQDirTLVTagsLastName)
		bCity, cityTLV := whitePages2FieldString(&inBody.TLVList, wire.ICQTLVTagsHomeCityName)
		bState, stateTLV := whitePages2FieldString(&inBody.TLVList, wire.ICQTLVTagsHomeStateAbbr)
		bNick = strings.TrimSpace(bNick)
		bFirst = strings.TrimSpace(bFirst)
		bLast = strings.TrimSpace(bLast)
		bCity = strings.TrimSpace(bCity)
		bState = strings.TrimSpace(bState)

		hasNick := nickTLV && bNick != ""
		hasFirst := firstTLV && bFirst != ""
		hasLast := lastTLV && bLast != ""
		hasCity := cityTLV && bCity != ""
		hasState := stateTLV && bState != ""

		emailStr, emailTLV := whitePages2FieldString(&inBody.TLVList, wire.ICQTLVTagsEmail)
		emailStr = strings.TrimSpace(emailStr)
		hasEmail := emailTLV && emailStr != ""

		hasName := hasNick || hasFirst || hasLast
		hasLoc := hasCity || hasState

		if hasEmail {
			wildPat := icqWildcardToSQLLike(emailStr)
			if (strings.ContainsAny(emailStr, "*?") || strings.Contains(wildPat, "%")) && wildPat != "" {
				if ef, ok := s.userFinder.(icqEmailPatternFinder); ok {
					return ef.FindByICQEmailPattern(ctx, wildPat)
				}
			}
			u, err := s.userFinder.FindByICQEmail(ctx, emailStr)
			if err != nil {
				if errors.Is(err, state.ErrNoUser) {
					return []state.User{}, nil
				}
				return nil, fmt.Errorf("FindByICQEmail (whitepages2): %w", err)
			}
			return []state.User{u}, nil
		}

		if hasName || hasLoc {
			if s.logger != nil {
				s.logger.Debug("FindByWhitePages2 name/location search",
					"hasFirst", hasFirst, "first", bFirst,
					"hasLast", hasLast, "last", bLast,
					"hasNick", hasNick, "nick", bNick,
					"hasCity", hasCity, "city", bCity,
					"hasState", hasState, "state", bState)
			}

			fp := icqWildcardToSQLLike(bFirst)
			lp := icqWildcardToSQLLike(bLast)
			np := icqWildcardToSQLLike(bNick)
			cp := icqWildcardToSQLLike(bCity)
			sp := icqWildcardToSQLLike(bState)
			if fp != "" && !strings.Contains(fp, "%") {
				fp += "%"
			}
			if lp != "" && !strings.Contains(lp, "%") {
				lp += "%"
			}
			if np != "" && !strings.Contains(np, "%") {
				np += "%"
			}
			if cp != "" && !strings.Contains(cp, "%") {
				cp += "%"
			}
			if sp != "" && !strings.Contains(sp, "%") {
				sp += "%"
			}

			if wf, ok := s.userFinder.(icqWhitePages2PatternFinder); ok && hasLoc {
				res, err := wf.FindByICQNameAndHomeLocationPattern(ctx, fp, lp, np, cp, sp)
				if err != nil {
					return nil, fmt.Errorf("FindByICQNameAndHomeLocationPattern failed: %w", err)
				}
				return res, nil
			}

			var res []state.User
			var err error
			if hasName {
				if pf, ok := s.userFinder.(icqNamePatternFinder); ok && (fp != "" || lp != "" || np != "") {
					res, err = pf.FindByICQNamePattern(ctx, fp, lp, np)
				} else {
					res, err = s.userFinder.FindByICQName(ctx, bFirst, bLast, bNick)
				}
				if err != nil {
					return nil, fmt.Errorf("FindByICQName failed: %w", err)
				}
			}
			if hasLoc {
				res = filterWhitePages2ByHomeCityState(res, bCity, bState)
			}
			return res, nil
		}

		if intCode, intKws, ok := decodeWhitePages2InterestsSearch(&inBody.TLVList); ok && len(intKws) > 0 {
			res, err := s.userFinder.FindByICQInterests(ctx, intCode, intKws)
			if err != nil {
				return nil, fmt.Errorf("FindByICQInterests (whitepages2): %w", err)
			}
			return res, nil
		}

		if uin, ok := decodeWhitePages2UINFromTLVs(&inBody.TLVList); ok {
			u, err := s.userFinder.FindByUIN(ctx, uin)
			if err != nil {
				if errors.Is(err, state.ErrNoUser) {
					return []state.User{}, nil
				}
				return nil, fmt.Errorf("FindByUIN (whitepages2 UIN TLV): %w", err)
			}
			return []state.User{u}, nil
		}

		return []state.User{}, nil
	}()

	resp := wire.ICQ_0x07DA_0x01AE_DBQueryMetaReplyLastUserFound{
		ICQMetadata: wire.ICQMetadata{
			UIN:     instance.UIN(),
			ReqType: wire.ICQDBQueryMetaReply,
			Seq:     seq,
		},
		Success:    wire.ICQStatusCodeOK,
		ReqSubType: wire.ICQDBQueryMetaReplyLastUserFound,
	}

	if err != nil {
		s.logger.Error("FindByWhitePages2 failed", "err", err.Error())
		resp.Success = wire.ICQStatusCodeErr
		resp.LastResult()
		return s.reply(ctx, instance, wire.ICQMessageReplyEnvelope{
			Message: resp,
		})
	}

	return s.sendICQUserSearchResults(ctx, instance, seq, users, "search_whitepages2_tlv")
}

// dirEmbSNACFlags matches ICQ6 directory embedded SNAC headers (see client
// DirectoryQuery payloads: flags 0x8000 after family/subtype).
const dirEmbSNACFlags uint16 = 0x8000

// dirQueryLooksLikeDirectorySearch reports whether extracted TLVs look like a
// real white-pages / directory lookup rather than the lightweight 0x0002
// self-check (which often embeds only the signing-in user's UIN).
func dirQueryLooksLikeDirectorySearch(q dirQueryCriteria, selfUIN uint32) bool {
	if q.FirstName != "" || q.LastName != "" || q.NickName != "" || q.Keyword != "" {
		return true
	}
	if q.UIN != 0 && q.UIN != selfUIN {
		return true
	}
	return false
}

func icqDirectoryEnvelopeMetadataHex(msg any) string {
	env := wire.ICQMessageReplyEnvelope{Message: msg}
	buf := bytes.Buffer{}
	// SNAC(0x15,0x02) TLV metadata uses MarshalBE on the envelope (ICQ inner fields stay LE).
	if err := wire.MarshalBE(&env, &buf); err != nil {
		return ""
	}
	return fmt.Sprintf("%X", buf.Bytes())
}

func (s ICQService) sendDirectoryQueryReply(ctx context.Context, instance *state.SessionInstance, phase string, msg any) error {
	if s.logger != nil {
		var metaSub uint16
		switch m := msg.(type) {
		case wire.ICQ_0x07DA_0x0FAA_DBQueryMetaReplyDirectoryData:
			metaSub = m.ReqSubType
		case wire.ICQ_0x07DA_0x0FB4_DBQueryMetaReplyDirectoryResponse:
			metaSub = m.ReqSubType
		}
		s.logger.Info("FindByDirectoryQuery response",
			"phase", phase,
			"meta_subtype_hex", fmt.Sprintf("0x%04X", metaSub),
			"metadata_envelope_hex", icqDirectoryEnvelopeMetadataHex(msg))
	}
	return s.replyWithHex(ctx, instance, wire.ICQMessageReplyEnvelope{Message: msg})
}

// sendDirectoryQuery0x0FB4Results is the buddy-list / multi-UIN path seen in
// mitschnitt suchen 3.pcapng: every frame uses outer 0x0FB4; embedded SNAC 0x05B9
// uses subtypes 0x0009 (row) then 0x0003 (terminator).
func (s ICQService) sendDirectoryQuery0x0FB4Results(ctx context.Context, instance *state.SessionInstance, seq uint16, dirHits []state.User, phase string) error {
	meta := wire.ICQMetadata{
		UIN:     instance.UIN(),
		ReqType: wire.ICQDBQueryMetaReply,
		Seq:     seq,
	}
	n := len(dirHits)
	if n == 0 {
		final := wire.ICQ_0x07DA_0x0FB4_DBQueryMetaReplyDirectoryResponse{
			ICQMetadata: meta,
			ReqSubType:  wire.ICQDBQueryMetaReplyDirectoryResponse,
			Success:     wire.ICQStatusCodeOK,
			Data:        buildDirQueryResponseData(nil, 0, 0x0003, dirEmbSNACFlags),
		}
		return s.sendDirectoryQueryReply(ctx, instance, phase+"_no_hits", final)
	}
	for i := 0; i < n; i++ {
		u := &dirHits[i]
		uin := u.IdentScreenName.UIN()
		row := wire.ICQ_0x07DA_0x0FB4_DBQueryMetaReplyDirectoryResponse{
			ICQMetadata: meta,
			ReqSubType:  wire.ICQDBQueryMetaReplyDirectoryResponse,
			Success:     wire.ICQStatusCodeOK,
			Data:        buildDirQueryResponseData(u, uin, 0x0009, dirEmbSNACFlags),
		}
		if err := s.sendDirectoryQueryReply(ctx, instance, fmt.Sprintf("%s_row_%d", phase, i), row); err != nil {
			return err
		}
	}
	final := wire.ICQ_0x07DA_0x0FB4_DBQueryMetaReplyDirectoryResponse{
		ICQMetadata: meta,
		ReqSubType:  wire.ICQDBQueryMetaReplyDirectoryResponse,
		Success:     wire.ICQStatusCodeOK,
		Data:        buildDirQueryResponseData(nil, 0, 0x0003, dirEmbSNACFlags),
	}
	return s.sendDirectoryQueryReply(ctx, instance, phase+"_terminator", final)
}

// AckDirectoryUpdate completes ICQDBQueryMetaReqDirectoryUpdate (0x0FD2). The
// client payload uses embedded SNAC 0x05B9/0x0003 (same subtype as the buddy
// directory terminator). Answering with MetaTerminalAck (0x07DA/0x01AE) leaves
// ICQ6 in a bad state after login directory self-check — see server_run.log
// immediately following successful 0x0FAA/0x0FB4 self-check replies.
func (s ICQService) AckDirectoryUpdate(ctx context.Context, instance *state.SessionInstance, seq uint16) error {
	meta := wire.ICQMetadata{
		UIN:     instance.UIN(),
		ReqType: wire.ICQDBQueryMetaReply,
		Seq:     seq,
	}
	final := wire.ICQ_0x07DA_0x0FB4_DBQueryMetaReplyDirectoryResponse{
		ICQMetadata: meta,
		ReqSubType:  wire.ICQDBQueryMetaReplyDirectoryResponse,
		Success:     wire.ICQStatusCodeOK,
		Data:        buildDirQueryResponseData(nil, 0, 0x0003, dirEmbSNACFlags),
	}
	if s.logger != nil {
		s.logger.Info("DirectoryUpdate ack",
			"uin", instance.UIN(),
			"seq", seq,
			"outer_meta_hex", fmt.Sprintf("0x%04X", wire.ICQDBQueryMetaReplyDirectoryResponse),
			"embedded_snac_subtype_hex", "0x0003",
		)
	}
	return s.replyWithHex(ctx, instance, wire.ICQMessageReplyEnvelope{Message: final})
}

// sendDirectoryQuery0x02RefSequence matches mitschnitt suchen 2.pcapng (ICQ6 →
// reference server): embedded SNAC subtype stays 0x0002 like the client request.
// Hit rows use outer META_DIRECTORY_DATA (0x0FAA); the closing frame uses
// META_DIRECTORY_RESPONSE (0x0FB4) with an empty embedded payload (nil user).
func (s ICQService) sendDirectoryQuery0x02RefSequence(ctx context.Context, instance *state.SessionInstance, seq uint16, dirHits []state.User, phase string) error {
	meta := wire.ICQMetadata{
		UIN:     instance.UIN(),
		ReqType: wire.ICQDBQueryMetaReply,
		Seq:     seq,
	}
	n := len(dirHits)
	if n == 0 {
		final := wire.ICQ_0x07DA_0x0FB4_DBQueryMetaReplyDirectoryResponse{
			ICQMetadata: meta,
			ReqSubType:  wire.ICQDBQueryMetaReplyDirectoryResponse,
			Success:     wire.ICQStatusCodeOK,
			Data:        buildDirQueryResponseData(nil, 0, 0x0002, dirEmbSNACFlags),
		}
		return s.sendDirectoryQueryReply(ctx, instance, phase+"_no_hits", final)
	}
	for i := 0; i < n; i++ {
		u := &dirHits[i]
		uin := u.IdentScreenName.UIN()
		row := wire.ICQ_0x07DA_0x0FAA_DBQueryMetaReplyDirectoryData{
			ICQMetadata: meta,
			ReqSubType:  wire.ICQDBQueryMetaReplyDirectoryData,
			Success:     wire.ICQStatusCodeOK,
			Data:        buildDirQueryResponseData(u, uin, 0x0002, dirEmbSNACFlags),
		}
		if err := s.sendDirectoryQueryReply(ctx, instance, fmt.Sprintf("%s_row_%d", phase, i), row); err != nil {
			return err
		}
	}
	final := wire.ICQ_0x07DA_0x0FB4_DBQueryMetaReplyDirectoryResponse{
		ICQMetadata: meta,
		ReqSubType:  wire.ICQDBQueryMetaReplyDirectoryResponse,
		Success:     wire.ICQStatusCodeOK,
		Data:        buildDirQueryResponseData(nil, 0, 0x0002, dirEmbSNACFlags),
	}
	return s.sendDirectoryQueryReply(ctx, instance, phase+"_terminator", final)
}

// alignDirectoryQueryBody returns a slice beginning at the embedded directory
// SNAC (BE family 0x05B9). Some clients prefix the payload with a LE length or
// a stray byte; reading subtype from offset 0 then leaves embSubtype at 0 and
// breaks search/self-check routing (wrong reply shape for ICQ6).
func alignDirectoryQueryBody(raw []byte) []byte {
	if len(raw) >= 4 && raw[2] == 0x05 && raw[3] == 0xb9 {
		return raw[2:]
	}
	if len(raw) >= 2 && raw[0] == 0x05 && raw[1] == 0xb9 {
		return raw
	}
	// Any embedded directory family 0x05B9 (subtype varies by client/build).
	for i := 0; i+2 <= len(raw); i++ {
		if raw[i] == 0x05 && raw[i+1] == 0xb9 {
			return raw[i:]
		}
	}
	return raw
}

func (s ICQService) FindByDirectoryQuery(ctx context.Context, instance *state.SessionInstance, rawBody []byte, seq uint16) error {
	// Client bodies often begin with a LE uint16 outer length, then a BE embedded
	// SNAC (family 0x05B9, subtype 0x0002 self-check or 0x0006 search, …).
	dirBody := alignDirectoryQueryBody(rawBody)
	var embSubtype uint16
	if len(dirBody) >= 4 && binary.BigEndian.Uint16(dirBody[0:2]) == 0x05B9 {
		embSubtype = binary.BigEndian.Uint16(dirBody[2:4])
	}
	query := extractDirQueryCriteria(dirBody)
	if u := pickDirectorySearchUIN(dirBody, instance.UIN()); u != 0 {
		query.UIN = u
	}
	// Collect every candidate UIN in the request body so that a bulk
	// resolve request (ICQ6 sends one DirectoryQuery with N UIN TLVs to
	// fill in buddy details) returns N hits instead of just one.
	uinCandidates := collectDirQueryUINCandidates(dirBody)
	hasTextCriteria := query.FirstName != "" || query.LastName != "" || query.NickName != "" || query.Keyword != ""
	// Buddy-list refresh uses 0x0006 with several ASCII UIN TLVs and empty name
	// slots. If the client also sent nickname/first/last/keyword TLVs, that is a
	// directory search — do not take the bulk multi-row path.
	bulkUINLookup := embSubtype == 0x0006 && len(uinCandidates) > 1 && !hasTextCriteria
	effectiveSearch := embSubtype == 0x0006 ||
		(embSubtype == 0x0002 && dirQueryLooksLikeDirectorySearch(query, instance.UIN()))

	if s.logger != nil {
		s.logger.Info("FindByDirectoryQuery called",
			"emb_subtype_hex", fmt.Sprintf("0x%04X", embSubtype),
			"effective_search", effectiveSearch,
			"query_uin", query.UIN,
			"query_first", query.FirstName,
			"query_nick", query.NickName,
			"raw_hex", fmt.Sprintf("%X", rawBody))
	}

	dirHits := make([]state.User, 0, icqSearchMaxResults)
	// ICQ 6 uses 0x0002 as a lightweight directory/self-check request and
	// 0x0006 for the actual search request. Returning full results on a plain
	// 0x0002 self-check can confuse the client; some builds still send 0x0002
	// with real search TLVs — treat those like 0x0006.
	if effectiveSearch {
		switch {
		case bulkUINLookup:
			// Buddy-list "fill in details" path: resolve every requested UIN
			// (skip the searcher's own UIN and de-dupe).
			seen := make(map[uint32]struct{}, len(uinCandidates))
			for _, u := range uinCandidates {
				if u == 0 || u == instance.UIN() {
					continue
				}
				if _, ok := seen[u]; ok {
					continue
				}
				seen[u] = struct{}{}
				if len(dirHits) >= icqSearchMaxResults {
					break
				}
				usr, err := s.userFinder.FindByUIN(ctx, u)
				if err != nil {
					if !errors.Is(err, state.ErrNoUser) && s.logger != nil {
						s.logger.Warn("FindByDirectoryQuery: bulk FindByUIN failed", "uin", u, "err", err)
					}
					continue
				}
				dirHits = append(dirHits, usr)
			}
		case query.FirstName != "" || query.LastName != "" || query.NickName != "" || query.Keyword != "":
			nick := query.NickName
			if nick == "" {
				nick = query.Keyword
			}
			// Same story as FindByWhitePages2: ICQ6 sends bare names, so an
			// exact SQL match never hits. Prefer a LIKE prefix search (auto
			// appending % when no explicit wildcard is present) and fall
			// back to exact match only if the store doesn't implement the
			// pattern finder (e.g. a slimmed-down test double).
			fp := icqWildcardToSQLLike(query.FirstName)
			lp := icqWildcardToSQLLike(query.LastName)
			np := icqWildcardToSQLLike(nick)
			if fp != "" && !strings.Contains(fp, "%") {
				fp += "%"
			}
			if lp != "" && !strings.Contains(lp, "%") {
				lp += "%"
			}
			if np != "" && !strings.Contains(np, "%") {
				np += "%"
			}
			var users []state.User
			var err error
			if pf, ok := s.userFinder.(icqNamePatternFinder); ok && (fp != "" || lp != "" || np != "") {
				users, err = pf.FindByICQNamePattern(ctx, fp, lp, np)
			} else {
				users, err = s.userFinder.FindByICQName(ctx, query.FirstName, query.LastName, nick)
			}
			if err == nil && len(users) == 0 && (nick != "" || query.Keyword != "") {
				needle := nick
				if needle == "" {
					needle = query.Keyword
				}
				type nickContainsFinder interface {
					FindByICQNameNickContains(ctx context.Context, needle string, limit int) ([]state.User, error)
				}
				if nf, ok := s.userFinder.(nickContainsFinder); ok {
					users, err = nf.FindByICQNameNickContains(ctx, needle, 20)
				}
			}
			if err == nil && len(users) > 0 {
				for i, u := range users {
					if i >= icqSearchMaxResults {
						break
					}
					dirHits = append(dirHits, u)
				}
			}
		case query.UIN > 0:
			u, err := s.userFinder.FindByUIN(ctx, query.UIN)
			if err != nil {
				if !errors.Is(err, state.ErrNoUser) {
					s.logger.Warn("FindByDirectoryQuery: FindByUIN failed", "uin", query.UIN, "err", err)
				}
			} else {
				dirHits = append(dirHits, u)
			}
		}
	}

	var user *state.User
	if len(dirHits) > 0 {
		u := dirHits[len(dirHits)-1]
		user = &u
	}

	// ICQ6 self-check: same two-frame shape as directory UIN search in
	// mitschnitt suchen 2.pcapng (0x0FAA + 0x0FB4, embedded 0x0002 throughout).
	if embSubtype == 0x0002 && !effectiveSearch {
		if user == nil {
			if u, err := s.userFinder.FindByUIN(ctx, instance.UIN()); err == nil {
				user = &u
			}
		}
		var selfHits []state.User
		if user != nil {
			selfHits = append(selfHits, *user)
		}
		s.logger.Debug("FindByDirectoryQuery self-check, replying 0x0FAA/0x0FB4 + embedded 0x0002")
		return s.sendDirectoryQuery0x02RefSequence(ctx, instance, seq, selfHits, "selfcheck")
	}

	if effectiveSearch {
		// Bulk UIN lookup (embSubtype 0x0006 with N UINs) is the
		// buddy-list "fill details" path, not a user-visible search.
		// Keep it on the custom 0x0FB4 path — ICQ6 parses the response
		// internally and doesn't show a search dialog for it.
		if bulkUINLookup {
			return s.sendDirectoryQuery0x0FB4Results(ctx, instance, seq, dirHits, "bulk")
		}
		// DirectoryQuery effective search (embedded 0x0002 UIN/name UI or 0x0006):
		// tmp3.txt / official server uses outer 0x0FB4 throughout with embedded
		// 0x05B9 subtypes 0x0009 per row then 0x0003 terminator (not 0x0FAA + 0x0002).
		return s.sendDirectoryQuery0x0FB4Results(ctx, instance, seq, dirHits, "search")
	}

	// Mis-parsed or unusual DirectoryQuery: still answer with the normal empty
	// directory terminator (0x0FB4 + embedded 0x0003), not a single 0x0009/no-row blob.
	return s.sendDirectoryQuery0x0FB4Results(ctx, instance, seq, dirHits, "non_search_fallback")
}

// buildDirQueryResponseData constructs the raw BE payload for the embedded
// directory SNAC (family 0x05B9 + body) placed in ICQ_0x07DA_* .Data.
//
// Wire layout:
//
//	[10 B] embedded SNAC header BE: family=0x05B9, subtype varies, flags=0x8000, ref=0
//	[1 B]  requestResult = 1 (success)
//	[2 B]  errLen = 0
//	[16 B] unknown (zeros)
//	[4 B]  itemCount
//	[2 B]  pageCount = 1
//	[2 B]  blockCount (0 = no results, 1 = has results)
//	[2 B]  itemLen (only present when blockCount = 1)
//	[N B]  TLV chain (only present when blockCount = 1)
func buildDirQueryResponseData(user *state.User, uin uint32, responseSubType uint16, embFlags uint16) []byte {
	buf := &bytes.Buffer{}
	be := binary.BigEndian

	// Embedded SNAC header (10 bytes, all BE)
	// family = 0x05B9 (ICQ directory service), subtype depends on request kind.
	binary.Write(buf, be, uint16(0x05B9))
	binary.Write(buf, be, responseSubType)
	binary.Write(buf, be, embFlags)       // flags (ICQ6 uses 0x8000 on directory queries)
	binary.Write(buf, be, uint16(0x0000)) // ref1
	binary.Write(buf, be, uint16(0x0000)) // ref2

	// requestResult = 1 (success)
	buf.WriteByte(0x01)

	// errLen = 0 (no error message)
	binary.Write(buf, be, uint16(0))

	// 16 bytes unknown (zeros)
	buf.Write(make([]byte, 16))

	if user == nil {
		// No results found
		binary.Write(buf, be, uint32(0)) // itemCount = 0
		binary.Write(buf, be, uint16(1)) // pageCount = 1
		binary.Write(buf, be, uint16(0)) // blockCount = 0 (no results)
		return buf.Bytes()
	}

	// Build a minimal BE TLV chain. ICQ6 directory hits use UTF-16 names then ASCII
	// UIN in TLV 0x0009 (official server; see mitschnitt suchen 4 *.pcapng). Buddy
	// bulk requests may still use 0x0032 in the wire, but responses mirror 0x0009.
	tlvBuf := &bytes.Buffer{}
	writeDirTLVUTF16BE(tlvBuf, wire.ICQDirTLVTagsFirstName, user.ICQBasicInfo.FirstName)
	writeDirTLVUTF16BE(tlvBuf, wire.ICQDirTLVTagsLastName, user.ICQBasicInfo.LastName)
	writeDirTLVUTF16BE(tlvBuf, wire.ICQDirTLVTagsNickname, user.ICQBasicInfo.Nickname)
	writeDirTLVStr(tlvBuf, wire.ICQDirTLVTagsUINASCII, strconv.FormatUint(uint64(uin), 10))

	binary.Write(buf, be, uint32(1))            // itemCount = 1
	binary.Write(buf, be, uint16(1))            // pageCount = 1
	binary.Write(buf, be, uint16(1))            // blockCount = 1
	binary.Write(buf, be, uint16(tlvBuf.Len())) // itemLen
	buf.Write(tlvBuf.Bytes())

	return buf.Bytes()
}

// writeDirTLVStr writes a BE TLV with a string value. Skips empty strings.
func writeDirTLVStr(buf *bytes.Buffer, typ uint16, value string) {
	if value == "" {
		return
	}
	binary.Write(buf, binary.BigEndian, typ)
	binary.Write(buf, binary.BigEndian, uint16(len(value)))
	buf.WriteString(value)
}

// writeDirTLVUTF16BE writes a BE TLV whose value is UCS-2 / UTF-16 BE (no BOM), as
// used by ICQ6 directory payloads for human-readable fields.
func writeDirTLVUTF16BE(buf *bytes.Buffer, typ uint16, value string) {
	if value == "" {
		return
	}
	encoded := utf16.Encode([]rune(value))
	raw := make([]byte, 2*len(encoded))
	for i, u := range encoded {
		binary.BigEndian.PutUint16(raw[i*2:], u)
	}
	binary.Write(buf, binary.BigEndian, typ)
	binary.Write(buf, binary.BigEndian, uint16(len(raw)))
	buf.Write(raw)
}

// writeDirTLVU16 writes a BE TLV with a uint16 value.
func writeDirTLVU16(buf *bytes.Buffer, typ uint16, value uint16) {
	binary.Write(buf, binary.BigEndian, typ)
	binary.Write(buf, binary.BigEndian, uint16(2))
	binary.Write(buf, binary.BigEndian, value)
}

func (s ICQService) FindByUIN(ctx context.Context, instance *state.SessionInstance, inBody wire.ICQ_0x07D0_0x051F_DBQueryMetaReqSearchByUIN, seq uint16) error {
	resp := wire.ICQ_0x07DA_0x01AE_DBQueryMetaReplyLastUserFound{
		ICQMetadata: wire.ICQMetadata{
			UIN:     instance.UIN(),
			ReqType: wire.ICQDBQueryMetaReply,
			Seq:     seq,
		},
		ReqSubType: wire.ICQDBQueryMetaReplyLastUserFound,
		Success:    wire.ICQStatusCodeOK,
	}
	resp.LastResult()

	res, err := s.userFinder.FindByUIN(ctx, inBody.UIN)

	switch {
	case errors.Is(err, state.ErrNoUser):
		resp.Success = wire.ICQStatusCodeFail
	case err != nil:
		s.logger.Error("FindByUIN failed", "err", err.Error())
		resp.Success = wire.ICQStatusCodeErr
	default:
		resp.Success = wire.ICQStatusCodeOK
		resp.Details = s.createResult(res)
	}

	return s.reply(ctx, instance, wire.ICQMessageReplyEnvelope{
		Message: resp,
	})
}

func (s ICQService) FindByUIN2(ctx context.Context, instance *state.SessionInstance, inBody wire.ICQ_0x07D0_0x0569_DBQueryMetaReqSearchByUIN2, seq uint16) error {
	UIN, hasUIN := inBody.Uint32LE(wire.ICQTLVTagsUIN)
	if !hasUIN {
		return errors.New("unable to get UIN from request")
	}

	resp := wire.ICQ_0x07DA_0x01AE_DBQueryMetaReplyLastUserFound{
		ICQMetadata: wire.ICQMetadata{
			UIN:     instance.UIN(),
			ReqType: wire.ICQDBQueryMetaReply,
			Seq:     seq,
		},
		ReqSubType: wire.ICQDBQueryMetaReplyLastUserFound,
		Success:    wire.ICQStatusCodeOK,
	}
	resp.LastResult()

	res, err := s.userFinder.FindByUIN(ctx, UIN)

	switch {
	case errors.Is(err, state.ErrNoUser):
		resp.Success = wire.ICQStatusCodeFail
	case err != nil:
		s.logger.Error("FindByUIN failed", "err", err.Error())
		resp.Success = wire.ICQStatusCodeErr
	default:
		resp.Success = wire.ICQStatusCodeOK
		resp.Details = s.createResult(res)
	}

	return s.reply(ctx, instance, wire.ICQMessageReplyEnvelope{
		Message: resp,
	})
}

func (s ICQService) FullUserInfo(ctx context.Context, instance *state.SessionInstance, inBody wire.ICQ_0x07D0_0x051F_DBQueryMetaReqSearchByUIN, seq uint16) error {

	user, err := s.userFinder.FindByUIN(ctx, inBody.UIN)
	if err != nil {
		return err
	}

	if err := s.userInfo(ctx, instance, user, seq); err != nil {
		return err
	}

	if err := s.moreUserInfo(ctx, instance, user, seq); err != nil {
		return err
	}

	if err := s.extraEmails(ctx, instance, user, seq); err != nil {
		return err
	}

	if err := s.homepageCat(ctx, instance, user, seq); err != nil {
		return err
	}

	if err := s.workInfo(ctx, instance, user, seq); err != nil {
		return err
	}

	if err := s.notes(ctx, instance, user, seq); err != nil {
		return err
	}

	if err := s.interests(ctx, instance, user, seq); err != nil {
		return err
	}

	if err := s.affiliations(ctx, instance, user, seq); err != nil {
		return err
	}
	return nil
}

func (s ICQService) OfflineMsgReq(ctx context.Context, instance *state.SessionInstance, seq uint16) error {
	messages, err := s.offlineMessageManager.RetrieveMessages(ctx, instance.IdentScreenName())
	if err != nil {
		return fmt.Errorf("retrieving messages: %w", err)
	}

	for _, msgIn := range messages {
		reply := wire.ICQ_0x0041_DBQueryOfflineMsgReply{
			ICQMetadata: wire.ICQMetadata{
				UIN:     instance.UIN(),
				ReqType: wire.ICQDBQueryOfflineMsgReply,
				Seq:     seq,
			},
			SenderUIN: msgIn.Sender.UIN(),
			Year:      uint16(msgIn.Sent.Year()),
			Month:     uint8(msgIn.Sent.Month()),
			Day:       uint8(msgIn.Sent.Day()),
			Hour:      uint8(msgIn.Sent.Hour()),
			Minute:    uint8(msgIn.Sent.Minute()),
		}

		switch msgIn.Message.ChannelID {
		case wire.ICBMChannelIM:
			if payload, hasIM := msgIn.Message.Bytes(wire.ICBMTLVAOLIMData); hasIM {
				// send regular IM
				msgText, err := wire.UnmarshalICBMMessageText(payload)
				if err != nil {
					return fmt.Errorf("unmarshalling offline message: %w", err)
				}
				reply.MsgType = wire.ICBMExtendedMsgTypePlain
				reply.Message = msgText
			}
		case wire.ICBMChannelICQ:
			if b, hasAuthReq := msgIn.Message.Bytes(wire.ICBMTLVData); hasAuthReq {
				msg := wire.ICBMCh4Message{}
				buf := bytes.NewBuffer(b)
				if err := wire.UnmarshalLE(&msg, buf); err != nil {
					return err
				}
				if instance.Session().UsesFeedbag() {
					// send auth grant/deny/request SNACs instead of the legacy MSG_TYPE_*
					// ICQ messages.
					frame := wire.SNACFrame{
						FoodGroup: wire.ICBM,
						SubGroup:  wire.ICBMChannelMsgToHost,
					}
					// fake a session since the sender may be offline
					sender := state.NewSession()
					sender.SetIdentScreenName(msgIn.Sender)
					if _, err := s.icbmSender(ctx, sender.AddInstance(), frame, msgIn.Message); err != nil {
						return fmt.Errorf("s.icbmSender: %w", err)
					}
					continue // do not send these messages in response
				}
				reply.MsgType = msg.MessageType
				reply.Flags = msg.Flags
				reply.Message = msg.Message
			}
		}

		if reply.MsgType == 0 {
			return fmt.Errorf("did not find an appropriate saved message payload. channel: %d",
				msgIn.Message.ChannelID)
		}

		msgOut := wire.ICQMessageReplyEnvelope{
			Message: reply,
		}
		if err := s.reply(ctx, instance, msgOut); err != nil {
			return fmt.Errorf("sending offline message: %w", err)
		}
	}

	eofMsg := wire.ICQMessageReplyEnvelope{
		Message: wire.ICQ_0x0042_DBQueryOfflineMsgReplyLast{
			ICQMetadata: wire.ICQMetadata{
				UIN:     instance.UIN(),
				ReqType: wire.ICQDBQueryOfflineMsgReplyLast,
				Seq:     seq,
			},
			DroppedMessages: 0,
		},
	}

	if err := s.reply(ctx, instance, eofMsg); err != nil {
		return fmt.Errorf("sending end of offline messages: %w", err)
	}

	return nil
}

func (s ICQService) SetAffiliations(ctx context.Context, instance *state.SessionInstance, inBody wire.ICQ_0x07D0_0x041A_DBQueryMetaReqSetAffiliations, seq uint16) error {
	if len(inBody.PastAffiliations) != 3 || len(inBody.Affiliations) != 3 {
		return fmt.Errorf("%w: expected 3 past affiliations and 3 affiliations", errICQBadRequest)
	}
	u := state.ICQAffiliations{
		PastCode1:       inBody.PastAffiliations[0].Code,
		PastKeyword1:    inBody.PastAffiliations[0].Keyword,
		PastCode2:       inBody.PastAffiliations[1].Code,
		PastKeyword2:    inBody.PastAffiliations[1].Keyword,
		PastCode3:       inBody.PastAffiliations[2].Code,
		PastKeyword3:    inBody.PastAffiliations[2].Keyword,
		CurrentCode1:    inBody.Affiliations[0].Code,
		CurrentKeyword1: inBody.Affiliations[0].Keyword,
		CurrentCode2:    inBody.Affiliations[1].Code,
		CurrentKeyword2: inBody.Affiliations[1].Keyword,
		CurrentCode3:    inBody.Affiliations[2].Code,
		CurrentKeyword3: inBody.Affiliations[2].Keyword,
	}

	if err := s.userUpdater.SetAffiliations(ctx, instance.IdentScreenName(), u); err != nil {
		return err
	}

	return s.reqAck(ctx, instance, seq, wire.ICQDBQueryMetaReplySetAffiliations)
}

func (s ICQService) SetBasicInfo(ctx context.Context, instance *state.SessionInstance, inBody wire.ICQ_0x07D0_0x03EA_DBQueryMetaReqSetBasicInfo, seq uint16) error {
	u := state.ICQBasicInfo{
		CellPhone:    inBody.CellPhone,
		CountryCode:  inBody.CountryCode,
		EmailAddress: inBody.EmailAddress,
		FirstName:    inBody.FirstName,
		GMTOffset:    inBody.GMTOffset,
		Address:      inBody.HomeAddress,
		City:         inBody.City,
		Fax:          inBody.Fax,
		Phone:        inBody.Phone,
		State:        inBody.State,
		LastName:     inBody.LastName,
		Nickname:     inBody.Nickname,
		PublishEmail: inBody.PublishEmail == wire.ICQUserFlagPublishEmailYes,
		ZIPCode:      inBody.ZIP,
	}

	if err := s.userUpdater.SetBasicInfo(ctx, instance.IdentScreenName(), u); err != nil {
		return err
	}

	return s.reqAck(ctx, instance, seq, wire.ICQDBQueryMetaReplySetBasicInfo)
}

func (s ICQService) SetEmails(ctx context.Context, instance *state.SessionInstance, inBody wire.ICQ_0x07D0_0x040B_DBQueryMetaReqSetEmails, seq uint16) error {
	if len(inBody.Emails) > 0 {
		s.logger.Debug("adding additional emails is not yet supported")
	}
	return s.reqAck(ctx, instance, seq, wire.ICQDBQueryMetaReplySetEmails)
}

func (s ICQService) SetICQPhone(ctx context.Context, instance *state.SessionInstance, inBody wire.ICQ_0x07D0_0x0654_DBQueryMetaReqSetICQPhone, seq uint16) error {
	s.logger.Debug("received SetICQPhone request")
	return s.reqAck(ctx, instance, seq, wire.ICQDBQueryMetaReplySetICQPhone)
}

func (s ICQService) SetInterests(ctx context.Context, instance *state.SessionInstance, inBody wire.ICQ_0x07D0_0x0410_DBQueryMetaReqSetInterests, seq uint16) error {
	if len(inBody.Interests) != 4 {
		return fmt.Errorf("%w: expected 4 interests", errICQBadRequest)
	}
	u := state.ICQInterests{
		Code1:    inBody.Interests[0].Code,
		Keyword1: inBody.Interests[0].Keyword,
		Code2:    inBody.Interests[1].Code,
		Keyword2: inBody.Interests[1].Keyword,
		Code3:    inBody.Interests[2].Code,
		Keyword3: inBody.Interests[2].Keyword,
		Code4:    inBody.Interests[3].Code,
		Keyword4: inBody.Interests[3].Keyword,
	}

	if err := s.userUpdater.SetInterests(ctx, instance.IdentScreenName(), u); err != nil {
		return err
	}

	return s.reqAck(ctx, instance, seq, wire.ICQDBQueryMetaReplySetInterests)
}

func (s ICQService) SetMoreInfo(ctx context.Context, instance *state.SessionInstance, inBody wire.ICQ_0x07D0_0x03FD_DBQueryMetaReqSetMoreInfo, seq uint16) error {
	u := state.ICQMoreInfo{
		Gender:       inBody.Gender,
		HomePageAddr: inBody.HomePageAddr,
		BirthYear:    inBody.BirthYear,
		BirthMonth:   inBody.BirthMonth,
		BirthDay:     inBody.BirthDay,
		Lang1:        inBody.Lang1,
		Lang2:        inBody.Lang2,
		Lang3:        inBody.Lang3,
	}

	if err := s.userUpdater.SetMoreInfo(ctx, instance.IdentScreenName(), u); err != nil {
		return err
	}

	return s.reqAck(ctx, instance, seq, wire.ICQDBQueryMetaReplySetMoreInfo)
}

func (s ICQService) SetPermissions(ctx context.Context, instance *state.SessionInstance, inBody wire.ICQ_0x07D0_0x0424_DBQueryMetaReqSetPermissions, seq uint16) error {
	u := state.ICQPermissions{
		AuthRequired: inBody.Authorization == 0,
		WebAware:     inBody.WebAware == 1,
	}

	if err := s.userUpdater.SetPermissions(ctx, instance.IdentScreenName(), u); err != nil {
		return err
	}

	return s.reqAck(ctx, instance, seq, wire.ICQDBQueryMetaReplySetPermissions)
}

func (s ICQService) SetUserNotes(ctx context.Context, instance *state.SessionInstance, inBody wire.ICQ_0x07D0_0x0406_DBQueryMetaReqSetNotes, seq uint16) error {
	u := state.ICQUserNotes{
		Notes: inBody.Notes,
	}

	if err := s.userUpdater.SetUserNotes(ctx, instance.IdentScreenName(), u); err != nil {
		return err
	}

	return s.reqAck(ctx, instance, seq, wire.ICQDBQueryMetaReplySetNotes)
}

func (s ICQService) SetWorkInfo(ctx context.Context, instance *state.SessionInstance, inBody wire.ICQ_0x07D0_0x03F3_DBQueryMetaReqSetWorkInfo, seq uint16) error {
	icqWorkInfo := state.ICQWorkInfo{
		Company:        inBody.Company,
		Department:     inBody.Department,
		OccupationCode: inBody.OccupationCode,
		Position:       inBody.Position,
		Address:        inBody.Address,
		City:           inBody.City,
		CountryCode:    inBody.CountryCode,
		Fax:            inBody.Fax,
		Phone:          inBody.Phone,
		State:          inBody.State,
		WebPage:        inBody.WebPage,
		ZIPCode:        inBody.ZIP,
	}

	if err := s.userUpdater.SetWorkInfo(ctx, instance.IdentScreenName(), icqWorkInfo); err != nil {
		return err
	}

	return s.reqAck(ctx, instance, seq, wire.ICQDBQueryMetaReplySetWorkInfo)
}

func (s ICQService) ShortUserInfo(ctx context.Context, instance *state.SessionInstance, inBody wire.ICQ_0x07D0_0x04BA_DBQueryMetaReqShortInfo, seq uint16) error {
	user, err := s.userFinder.FindByUIN(ctx, inBody.UIN)
	if err != nil {
		return err
	}

	info := wire.ICQ_0x07DA_0x0104_DBQueryMetaReplyShortInfo{
		ICQMetadata: wire.ICQMetadata{
			UIN:     instance.UIN(),
			ReqType: wire.ICQDBQueryMetaReply,
			Seq:     seq,
		},
		ReqSubType: wire.ICQDBQueryMetaReplyShortInfo,
		Success:    wire.ICQStatusCodeOK,
		Nickname:   user.ICQBasicInfo.Nickname,
		FirstName:  user.ICQBasicInfo.FirstName,
		LastName:   user.ICQBasicInfo.LastName,
		Email:      user.ICQBasicInfo.EmailAddress,
		Gender:     uint8(user.ICQMoreInfo.Gender),
	}
	if !user.ICQPermissions.AuthRequired {
		info.Authorization = 1
	}

	msg := wire.ICQMessageReplyEnvelope{
		Message: info,
	}

	return s.reply(ctx, instance, msg)
}

// icqXMLMetaReplyBody builds a minimal well-formed XML document for SNAC
// 0x07D0/0x0898 → 0x07DA/0x08A2. ICQ6 issues ResolverXML / DataFilesURL requests
// during login; answering with ICQStatusCodeFail breaks IM/directory bootstrap
// even when directory SNACs are correct.
func icqXMLMetaReplyBody(req string) string {
	req = strings.TrimSpace(req)
	if req == "" {
		return `<?xml version="1.0"?><response/>`
	}
	if strings.Contains(req, "ResolverXML") {
		return `<?xml version="1.0"?><response><Resolver></Resolver></response>`
	}
	if strings.Contains(req, "DataFilesURL") {
		return `<?xml version="1.0"?><response><DataFilesURL></DataFilesURL></response>`
	}
	return `<?xml version="1.0"?><response/>`
}

func (s ICQService) XMLReqData(ctx context.Context, instance *state.SessionInstance, inBody wire.ICQ_0x07D0_0x0898_DBQueryMetaReqXMLReq, seq uint16) error {
	xmlOut := icqXMLMetaReplyBody(inBody.XMLRequest)
	if s.logger != nil {
		s.logger.Debug("ICQ XMLReqData",
			"req_len", len(inBody.XMLRequest),
			"reply_len", len(xmlOut),
		)
	}
	msg := wire.ICQMessageReplyEnvelope{
		Message: wire.ICQ_0x07DA_0x08A2_DBQueryMetaReplyXMLData{
			ICQMetadata: wire.ICQMetadata{
				UIN:     instance.UIN(),
				ReqType: wire.ICQDBQueryMetaReply,
				Seq:     seq,
			},
			ReqSubType: wire.ICQDBQueryMetaReplyXMLData,
			Success:    wire.ICQStatusCodeOK,
			XML:        xmlOut,
		},
	}
	return s.reply(ctx, instance, msg)
}

func (s ICQService) affiliations(ctx context.Context, instance *state.SessionInstance, user state.User, seq uint16) error {
	msg := wire.ICQMessageReplyEnvelope{
		Message: wire.ICQ_0x07DA_0x00FA_DBQueryMetaReplyAffiliations{
			ICQMetadata: wire.ICQMetadata{
				UIN:     instance.UIN(),
				ReqType: wire.ICQDBQueryMetaReply,
				Seq:     seq,
			},
			ReqSubType: wire.ICQDBQueryMetaReplyAffiliations,
			Success:    wire.ICQStatusCodeOK,
			ICQ_0x07D0_0x041A_DBQueryMetaReqSetAffiliations: wire.ICQ_0x07D0_0x041A_DBQueryMetaReqSetAffiliations{
				PastAffiliations: []struct {
					Code    uint16
					Keyword string `oscar:"len_prefix=uint16,nullterm"`
				}{
					{
						Code:    user.ICQAffiliations.PastCode1,
						Keyword: user.ICQAffiliations.PastKeyword1,
					},
					{
						Code:    user.ICQAffiliations.PastCode2,
						Keyword: user.ICQAffiliations.PastKeyword2,
					},
					{
						Code:    user.ICQAffiliations.PastCode3,
						Keyword: user.ICQAffiliations.PastKeyword3,
					},
				},
				Affiliations: []struct {
					Code    uint16
					Keyword string `oscar:"len_prefix=uint16,nullterm"`
				}{
					{
						Code:    user.ICQAffiliations.CurrentCode1,
						Keyword: user.ICQAffiliations.CurrentKeyword1,
					},
					{
						Code:    user.ICQAffiliations.CurrentCode2,
						Keyword: user.ICQAffiliations.CurrentKeyword2,
					},
					{
						Code:    user.ICQAffiliations.CurrentCode3,
						Keyword: user.ICQAffiliations.CurrentKeyword3,
					},
				},
			},
		},
	}

	return s.reply(ctx, instance, msg)
}

func (s ICQService) createResult(res state.User) wire.ICQUserSearchRecord {
	uin, _ := strconv.Atoi(res.IdentScreenName.String())

	searchRecord := wire.ICQUserSearchRecord{
		UIN:       uint32(uin),
		Nickname:  res.ICQBasicInfo.Nickname,
		FirstName: res.ICQBasicInfo.FirstName,
		LastName:  res.ICQBasicInfo.LastName,
		Email:     res.ICQBasicInfo.EmailAddress,
		Gender:    uint8(res.ICQMoreInfo.Gender),
		Age:       res.Age(s.timeNow),
	}
	if !res.ICQPermissions.AuthRequired {
		searchRecord.Authorization = 1
	}

	userSess := s.sessionRetriever.RetrieveSession(res.IdentScreenName)
	if userSess != nil {
		searchRecord.OnlineStatus = 1
	}
	return searchRecord
}

func (s ICQService) extraEmails(ctx context.Context, instance *state.SessionInstance, user state.User, seq uint16) error {
	msg := wire.ICQMessageReplyEnvelope{
		Message: wire.ICQ_0x07DA_0x00EB_DBQueryMetaReplyExtEmailInfo{
			ICQMetadata: wire.ICQMetadata{
				UIN:     instance.UIN(),
				ReqType: wire.ICQDBQueryMetaReply,
				Seq:     seq,
			},
			ReqSubType: wire.ICQDBQueryMetaReplyExtEmailInfo,
			Success:    wire.ICQStatusCodeOK,
		},
	}

	return s.reply(ctx, instance, msg)
}

func (s ICQService) homepageCat(ctx context.Context, instance *state.SessionInstance, user state.User, seq uint16) error {
	msg := wire.ICQMessageReplyEnvelope{
		Message: wire.ICQ_0x07DA_0x010E_DBQueryMetaReplyHomePageCat{
			ICQMetadata: wire.ICQMetadata{
				UIN:     instance.UIN(),
				ReqType: wire.ICQDBQueryMetaReply,
				Seq:     seq,
			},
			ReqSubType: wire.ICQDBQueryMetaReplyHomePageCat,
			Success:    wire.ICQStatusCodeOK,
		},
	}

	return s.reply(ctx, instance, msg)
}

func (s ICQService) interests(ctx context.Context, instance *state.SessionInstance, user state.User, seq uint16) error {
	msg := wire.ICQMessageReplyEnvelope{
		Message: wire.ICQ_0x07DA_0x00F0_DBQueryMetaReplyInterests{
			ICQMetadata: wire.ICQMetadata{
				UIN:     instance.UIN(),
				ReqType: wire.ICQDBQueryMetaReply,
				Seq:     seq,
			},
			ReqSubType: wire.ICQDBQueryMetaReplyInterests,
			Success:    wire.ICQStatusCodeOK,
			Interests: []struct {
				Code    uint16
				Keyword string `oscar:"len_prefix=uint16,nullterm"`
			}{
				{
					Code:    user.ICQInterests.Code1,
					Keyword: user.ICQInterests.Keyword1,
				},
				{
					Code:    user.ICQInterests.Code2,
					Keyword: user.ICQInterests.Keyword2,
				},
				{
					Code:    user.ICQInterests.Code3,
					Keyword: user.ICQInterests.Keyword3,
				},
				{
					Code:    user.ICQInterests.Code4,
					Keyword: user.ICQInterests.Keyword4,
				},
			},
		},
	}

	return s.reply(ctx, instance, msg)
}

func (s ICQService) moreUserInfo(ctx context.Context, instance *state.SessionInstance, user state.User, seq uint16) error {
	msg := wire.ICQMessageReplyEnvelope{
		Message: wire.ICQ_0x07DA_0x00DC_DBQueryMetaReplyMoreInfo{
			ICQMetadata: wire.ICQMetadata{
				UIN:     instance.UIN(),
				ReqType: wire.ICQDBQueryMetaReply,
				Seq:     seq,
			},
			ReqSubType: wire.ICQDBQueryMetaReplyMoreInfo,
			Success:    wire.ICQStatusCodeOK,
			ICQ_0x07D0_0x03FD_DBQueryMetaReqSetMoreInfo: wire.ICQ_0x07D0_0x03FD_DBQueryMetaReqSetMoreInfo{
				Age:          uint8(user.Age(s.timeNow)),
				Gender:       user.ICQMoreInfo.Gender,
				HomePageAddr: user.ICQMoreInfo.HomePageAddr,
				BirthYear:    user.ICQMoreInfo.BirthYear,
				BirthMonth:   user.ICQMoreInfo.BirthMonth,
				BirthDay:     user.ICQMoreInfo.BirthDay,
				Lang1:        user.ICQMoreInfo.Lang1,
				Lang2:        user.ICQMoreInfo.Lang2,
				Lang3:        user.ICQMoreInfo.Lang3,
			},
			City:        user.ICQBasicInfo.City,
			State:       user.ICQBasicInfo.State,
			CountryCode: user.ICQBasicInfo.CountryCode,
			TimeZone:    user.ICQBasicInfo.GMTOffset,
		},
	}

	return s.reply(ctx, instance, msg)
}

func (s ICQService) notes(ctx context.Context, instance *state.SessionInstance, user state.User, seq uint16) error {
	msg := wire.ICQMessageReplyEnvelope{
		Message: wire.ICQ_0x07DA_0x00E6_DBQueryMetaReplyNotes{
			ICQMetadata: wire.ICQMetadata{
				UIN:     instance.UIN(),
				ReqType: wire.ICQDBQueryMetaReply,
				Seq:     seq,
			},
			ReqSubType: wire.ICQDBQueryMetaReplyNotes,
			Success:    wire.ICQStatusCodeOK,
			ICQ_0x07D0_0x0406_DBQueryMetaReqSetNotes: wire.ICQ_0x07D0_0x0406_DBQueryMetaReqSetNotes{
				Notes: user.ICQNotes.Notes,
			},
		},
	}

	return s.reply(ctx, instance, msg)
}

type dirQueryCriteria struct {
	UIN       uint32
	FirstName string
	LastName  string
	NickName  string
	Keyword   string
}

// dirTLVString decodes directory TLV string values. ICQ6 often sends UTF-16 BE
// (BMP: high byte 0 for Latin); plain ASCII is returned unchanged.
func dirTLVString(val []byte) string {
	if s, ok := icqTLVUTF16Payload(val); ok {
		return s
	}
	raw := strings.TrimRight(string(val), "\x00")
	if len(val) < 2 || len(val)%2 != 0 {
		return raw
	}
	runes := make([]rune, 0, len(val)/2)
	for j := 0; j+1 < len(val); j += 2 {
		r := rune(binary.BigEndian.Uint16(val[j : j+2]))
		if r == 0 {
			break
		}
		runes = append(runes, r)
	}
	u16 := string(runes)
	if u16 == "" || !utf8.ValidString(u16) {
		return raw
	}
	hasBMPLatinPattern := false
	for j := 0; j+1 < len(val); j += 2 {
		if val[j] == 0 && val[j+1] != 0 {
			hasBMPLatinPattern = true
			break
		}
	}
	if hasBMPLatinPattern {
		return u16
	}
	if utf8.ValidString(raw) && !strings.Contains(raw, "\x00") && len(raw) > 0 {
		return raw
	}
	return u16
}

// collectDirQueryASCIIUINs finds every BE TLV tag 0x0032 or 0x0009 (ASCII decimal UIN)
// in the buffer. ICQ6 directory frames are not always a clean single TLV
// stream from a fixed offset, so we rescan with byte-aligned tag detection.
func collectDirQueryASCIIUINs(b []byte) []uint32 {
	var out []uint32
	for i := 0; i+4 <= len(b); {
		tag := binary.BigEndian.Uint16(b[i : i+2])
		if tag != 0x0032 && tag != wire.ICQDirTLVTagsUINASCII {
			i++
			continue
		}
		length := int(binary.BigEndian.Uint16(b[i+2 : i+4]))
		if length < 5 || length > 12 || i+4+length > len(b) {
			i++
			continue
		}
		val := b[i+4 : i+4+length]
		if !isAllDigits(val) {
			i++
			continue
		}
		n, err := strconv.ParseUint(string(val), 10, 32)
		if err != nil || n == 0 {
			i++
			continue
		}
		out = append(out, uint32(n))
		i += 4 + length
	}
	return out
}

// collectDirQueryUINCandidates returns UINs from directory TLVs in buffer order.
// ICQ6 often sends multiple 0x0032 (ASCII UIN) TLVs (contact then self); using
// only the last one breaks white-pages lookup. UIN-search (embedded 0x0002) may use
// TLV 0x0009 for the same decimal string.
func collectDirQueryUINCandidates(b []byte) []uint32 {
	return collectDirQueryASCIIUINs(b)
}

// pickDirectorySearchUIN prefers the first TLV UIN that is not the searcher when
// multiple UIN TLVs are present; otherwise returns 0 to keep extractDirQueryCriteria's value.
func pickDirectorySearchUIN(dirBody []byte, selfUIN uint32) uint32 {
	cands := collectDirQueryUINCandidates(dirBody)
	if len(cands) <= 1 {
		return 0
	}
	for _, u := range cands {
		if u != 0 && u != selfUIN {
			return u
		}
	}
	if len(cands) > 0 {
		return cands[len(cands)-1]
	}
	return 0
}

// extractDirQueryCriteria scans a raw ICQ 6 DirectoryQuery body for TLVs
// commonly used for searching by UIN, name, or keyword.
//
// After the embedded directory SNAC (family 0x05B9, 10 bytes BE), the payload is
// not always a clean TLV stream for a naive forward walk. We skip that SNAC,
// run a best-effort BE TLV scan on the remainder, then always run the ASCII UIN
// length heuristic on the full buffer when UIN is still unknown.
func extractDirQueryCriteria(b []byte) dirQueryCriteria {
	out := dirQueryCriteria{}
	walk := b
	if len(b) >= 10 && binary.BigEndian.Uint16(b[0:2]) == 0x05B9 {
		walk = b[10:]
	}
	for i := 0; i+4 <= len(walk); {
		tag := binary.BigEndian.Uint16(walk[i : i+2])
		length := int(binary.BigEndian.Uint16(walk[i+2 : i+4]))
		if length == 0 || i+4+length > len(walk) {
			i++
			continue
		}
		val := walk[i+4 : i+4+length]
		valStr := dirTLVString(val)
		switch tag {
		case 0x0032, wire.ICQDirTLVTagsUINASCII: // UIN as ASCII
			if n, err := strconv.ParseUint(string(val), 10, 32); err == nil && n > 0 {
				out.UIN = uint32(n)
			}
		case 0x0064: // first name
			out.FirstName = valStr
		case 0x006E: // last name
			out.LastName = valStr
		case 0x0078: // nickname
			out.NickName = valStr
		case wire.ICQTLVTagsUIN:
			if n, ok := parseUint32LE(val); ok && n > 0 {
				out.UIN = n
			}
		case wire.ICQTLVTagsFirstName:
			out.FirstName = valStr
		case wire.ICQTLVTagsLastName:
			out.LastName = valStr
		case wire.ICQTLVTagsNickname:
			out.NickName = valStr
		case wire.ICQTLVTagsWhitepagesSearchKeywords:
			out.Keyword = valStr
		}
		i += 4 + length
	}
	if out.UIN == 0 {
		out.UIN = extractASCIIUINFallback(b)
	}
	return out
}

func parseUint32LE(v []byte) (uint32, bool) {
	if len(v) != 4 {
		return 0, false
	}
	return binary.LittleEndian.Uint32(v), true
}

func extractASCIIUINFallback(b []byte) uint32 {
	for i := 0; i+2 <= len(b); i++ {
		l := int(binary.BigEndian.Uint16(b[i : i+2]))
		if l < 5 || l > 12 || i+2+l > len(b) {
			continue
		}
		digits := b[i+2 : i+2+l]
		if !isAllDigits(digits) {
			continue
		}
		n, err := strconv.ParseUint(string(digits), 10, 32)
		if err == nil && n > 0 {
			return uint32(n)
		}
	}
	return 0
}

func isAllDigits(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, v := range b {
		if v < '0' || v > '9' {
			return false
		}
	}
	return true
}

func (s ICQService) reply(ctx context.Context, instance *state.SessionInstance, message wire.ICQMessageReplyEnvelope) error {
	rid, ok := icqctx.SNACRequestID(ctx)
	if s.logger != nil {
		s.logger.DebugContext(ctx, "ICQ DB reply SNAC",
			"to", instance.IdentScreenName().String(),
			"snac_request_id", rid,
			"snac_request_id_set", ok,
		)
	}
	msg := wire.SNACMessage{
		Frame: wire.SNACFrame{
			FoodGroup: wire.ICQ,
			SubGroup:  wire.ICQDBReply,
			RequestID: rid,
		},
		Body: wire.SNAC_0x15_0x02_DBReply{
			TLVRestBlock: wire.TLVRestBlock{
				TLVList: wire.TLVList{
					wire.NewTLVBE(wire.ICQTLVTagsMetadata, message),
				},
			},
		},
	}

	// Directory/meta replies are request/response packets and should always go
	// back to the current live instance, even if the session index is still
	// catching up during login race windows.
	s.messageRelayer.RelayToSelf(ctx, instance, msg)
	return nil
}

// replyWithHex is like reply but also logs the raw TLV bytes for diagnostics.
func (s ICQService) replyWithHex(ctx context.Context, instance *state.SessionInstance, message wire.ICQMessageReplyEnvelope) error {
	tlv := wire.NewTLVBE(wire.ICQTLVTagsMetadata, message)
	rid, ok := icqctx.SNACRequestID(ctx)
	if s.logger != nil {
		s.logger.DebugContext(ctx, "ICQ reply raw TLV",
			"tag", fmt.Sprintf("0x%04X", tlv.Tag),
			"value_hex", fmt.Sprintf("%X", tlv.Value),
			"snac_request_id", rid,
			"snac_request_id_set", ok,
		)
	}
	msg := wire.SNACMessage{
		Frame: wire.SNACFrame{
			FoodGroup: wire.ICQ,
			SubGroup:  wire.ICQDBReply,
			RequestID: rid,
		},
		Body: wire.SNAC_0x15_0x02_DBReply{
			TLVRestBlock: wire.TLVRestBlock{
				TLVList: wire.TLVList{tlv},
			},
		},
	}
	s.messageRelayer.RelayToSelf(ctx, instance, msg)
	return nil
}

func (s ICQService) reqAck(ctx context.Context, instance *state.SessionInstance, seq uint16, subType uint16) error {
	msg := wire.ICQMessageReplyEnvelope{
		Message: wire.ICQ_0x07DA_0x00DC_DBQueryMetaReplyMoreInfo{
			ICQMetadata: wire.ICQMetadata{
				UIN:     instance.UIN(),
				ReqType: wire.ICQDBQueryMetaReply,
				Seq:     seq,
			},
			ReqSubType: subType,
			Success:    wire.ICQStatusCodeOK,
		},
	}

	return s.reply(ctx, instance, msg)
}

func (s ICQService) userInfo(ctx context.Context, instance *state.SessionInstance, user state.User, seq uint16) error {
	userInfo := wire.ICQ_0x07DA_0x00C8_DBQueryMetaReplyBasicInfo{
		ICQMetadata: wire.ICQMetadata{
			UIN:     instance.UIN(),
			ReqType: wire.ICQDBQueryMetaReply,
			Seq:     seq,
		},
		ReqSubType:  wire.ICQDBQueryMetaReplyBasicInfo,
		Success:     wire.ICQStatusCodeOK,
		Nickname:    user.ICQBasicInfo.Nickname,
		FirstName:   user.ICQBasicInfo.FirstName,
		LastName:    user.ICQBasicInfo.LastName,
		Email:       user.ICQBasicInfo.EmailAddress,
		City:        user.ICQBasicInfo.City,
		State:       user.ICQBasicInfo.State,
		Phone:       user.ICQBasicInfo.Phone,
		Fax:         user.ICQBasicInfo.Fax,
		Address:     user.ICQBasicInfo.Address,
		CellPhone:   user.ICQBasicInfo.CellPhone,
		ZIP:         user.ICQBasicInfo.ZIPCode,
		CountryCode: user.ICQBasicInfo.CountryCode,
		GMTOffset:   user.ICQBasicInfo.GMTOffset,
		AuthFlag:    0, // required by default
		WebAware:    1,
		DCPerms:     0,
	}

	if !user.ICQPermissions.AuthRequired {
		userInfo.AuthFlag = 1
	}
	if user.ICQPermissions.WebAware {
		userInfo.WebAware = 1
	} else {
		userInfo.WebAware = 0
	}

	if user.ICQBasicInfo.PublishEmail {
		userInfo.PublishEmail = wire.ICQUserFlagPublishEmailYes
	} else {
		userInfo.PublishEmail = wire.ICQUserFlagPublishEmailNo
	}

	msg := wire.ICQMessageReplyEnvelope{
		Message: userInfo,
	}
	return s.reply(ctx, instance, msg)

}

func (s ICQService) workInfo(ctx context.Context, instance *state.SessionInstance, user state.User, seq uint16) error {
	msg := wire.ICQMessageReplyEnvelope{
		Message: wire.ICQ_0x07DA_0x00D2_DBQueryMetaReplyWorkInfo{
			ICQMetadata: wire.ICQMetadata{
				UIN:     instance.UIN(),
				ReqType: wire.ICQDBQueryMetaReply,
				Seq:     seq,
			},
			ReqSubType: wire.ICQDBQueryMetaReplyWorkInfo,
			Success:    wire.ICQStatusCodeOK,
			ICQ_0x07D0_0x03F3_DBQueryMetaReqSetWorkInfo: wire.ICQ_0x07D0_0x03F3_DBQueryMetaReqSetWorkInfo{
				City:           user.ICQWorkInfo.City,
				State:          user.ICQWorkInfo.State,
				Phone:          user.ICQWorkInfo.Phone,
				Fax:            user.ICQWorkInfo.Fax,
				Address:        user.ICQWorkInfo.Address,
				ZIP:            user.ICQWorkInfo.ZIPCode,
				CountryCode:    user.ICQWorkInfo.CountryCode,
				Company:        user.ICQWorkInfo.Company,
				Department:     user.ICQWorkInfo.Department,
				Position:       user.ICQWorkInfo.Position,
				OccupationCode: user.ICQWorkInfo.OccupationCode,
				WebPage:        user.ICQWorkInfo.WebPage,
			},
		},
	}
	return s.reply(ctx, instance, msg)
}
