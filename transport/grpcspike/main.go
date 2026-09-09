// Command grpcprobe is the rejected-alternative half of the transport spike.
//
// It deliberately lives OUTSIDE the TrendInvesting module so that measuring
// gRPC costs the repository no dependency. If the ADR had chosen gRPC, this
// code would move into transport/ and google.golang.org/grpc would be
// justified under docs/dependency-policy.md.
//
// There is no .proto file and no generated code: the method carries an opaque
// bytes field (wrapperspb.BytesValue) holding exactly the same envelope JSON
// the Unix-socket candidate sends. That is the point — it isolates the cost of
// the gRPC transport from the cost of changing the payload encoding, so the
// two candidates are compared on the same bytes.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"time"

	"github.com/richard-whittemore/TrendInvesting/transport/spike"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const fullMethod = "/trendinvesting.spike.Transport/Decide"

type decideService interface {
	Decide(context.Context, *wrapperspb.BytesValue) (*wrapperspb.BytesValue, error)
}

type engine struct {
	decide func(context.Context, any) (any, error)
}

var serviceDesc = grpc.ServiceDesc{
	ServiceName: "trendinvesting.spike.Transport",
	HandlerType: (*decideService)(nil),
	Methods: []grpc.MethodDesc{{
		MethodName: "Decide",
		Handler: func(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			in := new(wrapperspb.BytesValue)
			if err := dec(in); err != nil {
				return nil, err
			}
			return srv.(decideService).Decide(ctx, in)
		},
	}},
	Metadata: "spike",
}

func (e *engine) Decide(ctx context.Context, in *wrapperspb.BytesValue) (*wrapperspb.BytesValue, error) {
	out, err := e.decide(ctx, in.GetValue())
	if err != nil {
		return nil, err
	}
	return wrapperspb.Bytes(out.([]byte)), nil
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: grpcprobe serve|bench")
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "bench":
		err = bench(os.Args[2:])
	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
	if err != nil {
		log.Fatal(err)
	}
}

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	socket := flags.String("socket", "/tmp/trend-grpc.sock", "unix socket path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	_ = os.Remove(*socket)
	listener, err := net.Listen("unix", *socket)
	if err != nil {
		return err
	}
	decider := spike.Decider(time.Now)
	// internal/event cannot be named from outside the module, so an empty
	// envelope from the harness supplies the type to decode into.
	template := spike.NewBarEnvelope(0, 0, time.Now)
	server := grpc.NewServer(grpc.MaxRecvMsgSize(8<<20), grpc.MaxSendMsgSize(8<<20))
	server.RegisterService(&serviceDesc, &engine{decide: func(ctx context.Context, raw any) (any, error) {
		bar := template
		if err := json.Unmarshal(raw.([]byte), &bar); err != nil {
			return nil, err
		}
		if err := bar.Validate(); err != nil {
			return nil, err
		}
		decision, err := decider(ctx, bar)
		if err != nil {
			return nil, err
		}
		return json.Marshal(decision)
	}})
	fmt.Printf("grpc listening on %s\n", *socket)
	return server.Serve(listener)
}

func bench(args []string) error {
	flags := flag.NewFlagSet("bench", flag.ExitOnError)
	socket := flags.String("socket", "/tmp/trend-grpc.sock", "unix socket path")
	rounds := flags.Int("rounds", 500, "measured round trips")
	warmup := flags.Int("warmup", 50, "discarded round trips")
	universe := flags.Int("universe", 1000, "instruments per bar")
	label := flags.String("label", "go-grpc", "row label")
	if err := flags.Parse(args); err != nil {
		return err
	}
	conn, err := grpc.NewClient("unix://"+*socket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(8<<20), grpc.MaxCallSendMsgSize(8<<20)))
	if err != nil {
		return err
	}
	defer conn.Close()

	sequence := uint64(1)
	template := spike.NewBarEnvelope(0, 0, time.Now)
	exchange := func() (int, time.Duration, error) {
		bar := spike.NewBarEnvelope(sequence, *universe, time.Now)
		sequence++
		// Encoding is inside the timer to match transport.Client.Decide,
		// which marshals inside its own measured exchange.
		started := time.Now()
		encoded, err := json.Marshal(bar)
		if err != nil {
			return 0, 0, err
		}
		in := wrapperspb.Bytes(encoded)
		out := new(wrapperspb.BytesValue)
		if err := conn.Invoke(context.Background(), fullMethod, in, out); err != nil {
			return 0, 0, err
		}
		decision := template
		if err := json.Unmarshal(out.GetValue(), &decision); err != nil {
			return 0, 0, err
		}
		if decision.CausationID != bar.ID {
			return 0, 0, fmt.Errorf("reply cites %q, sent %q", decision.CausationID, bar.ID)
		}
		return len(encoded) + 1, time.Since(started), nil
	}

	for i := 0; i < *warmup; i++ {
		if _, _, err := exchange(); err != nil {
			return err
		}
	}
	samples := make([]time.Duration, 0, *rounds)
	requestBytes := 0
	for i := 0; i < *rounds; i++ {
		size, elapsed, err := exchange()
		if err != nil {
			return err
		}
		requestBytes = size
		samples = append(samples, elapsed)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	stats := spike.Summarise(samples)
	stats.RequestBytes = requestBytes
	fmt.Printf("%s universe=%d %s\n", *label, *universe, stats)
	return nil
}
