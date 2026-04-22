package icqctx

import "context"

type snacRequestIDKey struct{}

type whitePagesWildcardKey struct{}

// WithSNACRequestID stores the FLAP/SNAC request ID from the client's ICQDBQuery
// frame so ICQDBReply packets can echo it (OSCAR family 0x15).
func WithSNACRequestID(ctx context.Context, requestID uint32) context.Context {
	return context.WithValue(ctx, snacRequestIDKey{}, requestID)
}

// SNACRequestID returns the stored request ID and whether it was set.
func SNACRequestID(ctx context.Context) (uint32, bool) {
	v, ok := ctx.Value(snacRequestIDKey{}).(uint32)
	return v, ok
}

// WithWhitePagesPlainWildcard marks SNAC meta subtype 0x0551 (plain whitepages
// wildcard) so the service layer uses LIKE semantics for string criteria.
func WithWhitePagesPlainWildcard(ctx context.Context, on bool) context.Context {
	return context.WithValue(ctx, whitePagesWildcardKey{}, on)
}

// WhitePagesPlainWildcard reports whether the current request is 0x0551.
func WhitePagesPlainWildcard(ctx context.Context) bool {
	v, ok := ctx.Value(whitePagesWildcardKey{}).(bool)
	return ok && v
}
