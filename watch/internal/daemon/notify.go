package daemon

import (
	"os"

	"github.com/matteobortolazzo/cenci/watch/v2/internal/ipc"
)

// DeliverEvent sends a hook event without surfacing failures to the calling
// agent. If the default daemon is unavailable, it is started on demand and the
// exact event is retried once. Custom sockets are explicitly managed by their
// callers and never trigger default-daemon startup.
func DeliverEvent(socketPath string, event ipc.HookEvent) {
	deliverEvent(socketPath, event, ipc.SendEvent, EnsureRunning)
}

func deliverEvent(socketPath string, event ipc.HookEvent, send func(string, ipc.HookEvent) error, ensure func()) {
	// Inside a sandbox, EnsureRunning's CENCI_SANDBOX no-op means the retry
	// below can never self-heal — a terminal failure against the default
	// event socket is worth classifying and recording so `cenci diagnose`
	// can surface it host-side (#1122).
	sandboxDefaultSocket := os.Getenv("CENCI_SANDBOX") == "1" && socketPath == ipc.DefaultEventSocketPath()

	if err := send(socketPath, event); err == nil {
		if sandboxDefaultSocket {
			clearUndeliveredMarker()
		}
		return
	}
	if socketPath != ipc.DefaultEventSocketPath() {
		return
	}
	ensure()
	if err := send(socketPath, event); err == nil {
		if sandboxDefaultSocket {
			clearUndeliveredMarker()
		}
		return
	}
	if sandboxDefaultSocket {
		mountinfo, readErr := os.ReadFile(mountInfoPath)
		code, message := classifyDeliveryFailure(string(mountinfo), readErr == nil, socketPath)
		writeUndeliveredMarker(event.SessionID, code, message)
	}
}
