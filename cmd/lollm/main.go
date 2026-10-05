// Command lollm is the LoLLM single-binary AI gateway / LLM router.
package main

import (
	"os"

	"github.com/lollm/lollm/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		// cobra already printed the error message.
		os.Exit(1)
	}
}
