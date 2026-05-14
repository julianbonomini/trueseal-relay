# trueseal-relay is a separate repo from trueseal-sync

trueseal-relay (this repo) is the deployable relay server. trueseal-sync is the client library. They were split into separate repos because they have fundamentally different consumers and language requirements.

trueseal-relay is a Go binary — deployed as infrastructure, never imported as a library, never ported to other languages. trueseal-sync is a client SDK — imported by apps (trueseal-clip, trueseal-secrets, third-party developers), and will eventually need Swift and Kotlin implementations for iOS and Android.

Bundling a Go relay server inside a client library that must be portable to Swift and Kotlin is incoherent. The split makes trueseal-sync a clean, language-agnostic protocol with a Go reference implementation, and trueseal-relay a pure Go server.

The shared wire format (Protobuf envelope definitions) is owned by trueseal-sync as the protocol authority. trueseal-relay implements against it.
