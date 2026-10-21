// Command canonical_vector prints what internal/event CanonicalEnvelopeBytes
// renders for each envelope on stdin (one JSON envelope per line), so the
// Python readers' hand-written canonicaliser is held to Go's own output: one
// JSON object per line, {"canonical": <the bytes as text>, "sha256": <their
// hash>}.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/richard-whittemore/TrendInvesting/internal/event"
)

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1<<20), 1<<26)
	for scanner.Scan() {
		var e event.Envelope
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		canonical := event.CanonicalEnvelopeBytes(e)
		sum := sha256.Sum256(canonical)
		line, err := json.Marshal(map[string]string{"canonical": string(canonical), "sha256": hex.EncodeToString(sum[:])})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(string(line))
	}
}
