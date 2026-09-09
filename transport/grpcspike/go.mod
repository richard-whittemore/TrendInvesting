// This is a SEPARATE MODULE on purpose.
//
// It holds the rejected alternative from the transport spike (ADR 0014) so the
// measurement stays reproducible, without google.golang.org/grpc entering the
// main module's go.mod. `go build ./...` at the repository root does not
// descend into a directory with its own go.mod, so `make check` never compiles
// this and the main module's dependency surface is unchanged.
module github.com/richard-whittemore/TrendInvesting/transport/grpcspike

go 1.24.4

require (
	github.com/richard-whittemore/TrendInvesting v0.0.0
	google.golang.org/grpc v1.71.0
	google.golang.org/protobuf v1.36.8
)

require (
	golang.org/x/net v0.34.0 // indirect
	golang.org/x/sys v0.39.0 // indirect
	golang.org/x/text v0.31.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250115164207-1a7da9e5054f // indirect
)

replace github.com/richard-whittemore/TrendInvesting => ../..
