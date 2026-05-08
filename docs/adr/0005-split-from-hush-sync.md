# hush-relay is a separate repo from hush-sync

hush-relay (this repo) is the deployable relay server. hush-sync is the client library. They were split into separate repos because they have fundamentally different consumers and language requirements.

hush-relay is a Go binary — deployed as infrastructure, never imported as a library, never ported to other languages. hush-sync is a client SDK — imported by apps (hush-clip, hush-secrets, third-party developers), and will eventually need Swift and Kotlin implementations for iOS and Android.

Bundling a Go relay server inside a client library that must be portable to Swift and Kotlin is incoherent. The split makes hush-sync a clean, language-agnostic protocol with a Go reference implementation, and hush-relay a pure Go server.

The shared wire format (Protobuf envelope definitions) is owned by hush-sync as the protocol authority. hush-relay implements against it.
