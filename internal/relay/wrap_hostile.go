//go:build hostile

package relay

import (
	"context"
)

// wrapDelivery (hostile build): an installed DeliveryWrapper transforms the
// Receive Session's blob stream. No wrapper installed — the passthrough.
func (r *Router) wrapDelivery(ctx context.Context, key RecipientKey, ch <-chan DeliveryBlob) <-chan DeliveryBlob {
	if r.wrapper == nil {
		return ch
	}
	return r.wrapper.WrapSession(ctx, key, ch)
}

// SetDeliveryWrapper installs the delivery-stream wrapper (RELAY-14, the
// Hostile Relay). It exists ONLY in `hostile` tag builds: a release build
// has no symbol by which a wrapper could be installed, so the hooks cannot
// be enrolled at runtime. nil clears it.
func (r *Router) SetDeliveryWrapper(w DeliveryWrapper) {
	r.wrapper = w
}
