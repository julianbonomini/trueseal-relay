//go:build !hostile

package relay

import (
	"context"
)

// wrapDelivery (release build): an unconditional passthrough. There is no
// SetDeliveryWrapper in this build — no hook code exists to wrap with, and
// the compiler inlines this call away (RELAY-14: a released binary cannot
// carry the Hostile Relay surface; CI proves it with go tool nm).
func (r *Router) wrapDelivery(_ context.Context, _ RecipientKey, ch <-chan DeliveryBlob) <-chan DeliveryBlob {
	return ch
}
