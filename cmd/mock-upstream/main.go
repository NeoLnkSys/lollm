// Command mock-upstream runs a scripted OpenAI-compatible mock provider —
// handy for developing and demoing the gateway without real API keys.
//
//	./mock-upstream --addr 127.0.0.1:21999 --fail-first 1
//	./mock-upstream --addr 127.0.0.1:22000 --stream
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/lollm/lollm/internal/providers"
)

func main() {
	var (
		addr      = flag.String("addr", "127.0.0.1:21999", "listen address")
		name      = flag.String("name", "mock", "display name for logs")
		failFirst = flag.Int("fail-first", 0, "fail the first N chat requests with 429 + Retry-After 30")
		stream    = flag.Bool("stream", false, "answer with an SSE stream instead of JSON")
	)
	flag.Parse()

	var behaviors []providers.MockBehavior
	for i := 0; i < *failFirst; i++ {
		behaviors = append(behaviors, providers.MockRateLimit(30))
	}
	if *stream {
		behaviors = append(behaviors, providers.MockSSE())
	} else {
		behaviors = append(behaviors, providers.MockOK())
	}

	log.Printf("mock-upstream %q listening on http://%s (fail-first=%d stream=%v)",
		*name, *addr, *failFirst, *stream)
	if err := http.ListenAndServe(*addr, providers.NewMock(behaviors...).Handler()); err != nil {
		log.Fatal(err)
	}
}
