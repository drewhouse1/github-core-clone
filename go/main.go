// The forge's Go core: bind an ephemeral port, print the port for the
// Node launcher to read, then serve until killed. The launcher (main.ts)
// owns the register/done lifecycle with the trial env; this process only
// serves requests. Prints exactly one line to stdout: FORGE_LISTEN <port>.

package main

import (
	"fmt"
	"os"
)

func main() {
	store := NewStore(scenarioViewer())
	server := NewForgeServer(store)
	if err := server.Listen("0.0.0.0:0"); err != nil {
		fmt.Fprintln(os.Stderr, "server failed:", err)
		os.Exit(1)
	}
	fmt.Printf("FORGE_LISTEN %d\n", server.Addr())
	// Serve forever; the launcher kills this process (SIGTERM) when the
	// trial is done. No signal handler: the default SIGTERM death is the
	// clean shutdown path.
	select {}
}
