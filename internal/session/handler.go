package session

import (
	"context"

	"github.com/julianbonomini/hush-relay/internal/relay"
)

// Handler is the interface the routing loop implements.
// The session layer calls into it for each parsed frame — no routing logic
// lives in the session layer itself.
type Handler interface {
	// OnPush is called for each Push frame received on a Push Session.
	// envelope is the complete raw serialized proto Envelope bytes.
	// Returns an error to signal the relay should not Ack (e.g. store failed).
	OnPush(ctx context.Context, envelope []byte) error

	// OnReceiveConnect is called when a Receive Session is established.
	// deviceKey is the device's stable noise public key.
	// Returns a channel on which the routing loop sends DeliveryBlobs to deliver
	// to this device. The channel is closed when ctx is done.
	OnReceiveConnect(ctx context.Context, deviceKey relay.RecipientKey) <-chan relay.DeliveryBlob

	// OnDeliverAck is called when the Device sends a DeliverAck frame.
	// blobID is the opaque identifier echoed from the corresponding Deliver frame.
	// The implementation deletes the blob from the InboxStore. See ADR-0009.
	OnDeliverAck(ctx context.Context, deviceKey relay.RecipientKey, blobID int64) error
}
