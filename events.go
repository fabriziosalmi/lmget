package lmget

import "time"

// SchemaVersion is the version of the Event JSON schema emitted by --json.
// It changes only on breaking changes; new optional fields do not bump it.
const SchemaVersion = 1

type EventType string

const (
	EvResolve   EventType = "resolve"    // resolution started: Ref
	EvPlan      EventType = "plan"       // Plan resolved: what will be fetched
	EvFileStart EventType = "file_start" // File, Total
	EvProgress  EventType = "progress"   // File, Bytes of Total (about 10 per second)
	EvFileDone  EventType = "file_done"  // File, SHA256, Cached
	EvExport    EventType = "export"     // Path written for Target
	EvDone      EventType = "done"       // Bytes fetched from the network, Elapsed
	EvLock      EventType = "lock"       // Path of the lockfile written after the pull
	EvError     EventType = "error"      // Error, then the process exits with Error.Code
)

// Event is one line of the --json stream. Fields not relevant to a Type are
// omitted.
type Event struct {
	V       int       `json:"v"`
	Type    EventType `json:"type"`
	Time    time.Time `json:"time"`
	Ref     *Ref      `json:"ref,omitempty"`
	Plan    *Plan     `json:"plan,omitempty"`
	File    string    `json:"file,omitempty"`
	Bytes   int64     `json:"bytes,omitempty"`
	Total   int64     `json:"total,omitempty"`
	SHA256  string    `json:"sha256,omitempty"`
	Cached  bool      `json:"cached,omitempty"`
	Target  string    `json:"target,omitempty"`
	Path    string    `json:"path,omitempty"`
	Fetched int64     `json:"fetched,omitempty"`
	Elapsed float64   `json:"elapsed_s,omitempty"`
	Error   *Error    `json:"error,omitempty"`
}

func ev(t EventType) Event { return Event{V: SchemaVersion, Type: t, Time: time.Now().UTC()} }
