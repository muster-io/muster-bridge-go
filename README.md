# muster-bridge-go

The Go library for writing a bridge between [Muster](https://github.com/muster-io/muster) and a messenger it does not
support natively: the library serves Muster's bridge protocol (`api/bridge.yaml`) — tokens, validation, idempotency,
events and signed push — and the bridge author writes only the calls to their messenger. MIT licensed.
