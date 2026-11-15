package lmget

import (
	"context"
	"errors"
	"fmt"
)

// ExitCode is the process exit status of the lmget CLI. The values are part of
// the public contract: scripts and integrations may branch on them.
type ExitCode int

const (
	ExitOK          ExitCode = 0
	ExitFailure     ExitCode = 1   // anything not covered below
	ExitUsage       ExitCode = 2   // bad reference, flag or unsupported source
	ExitNotFound    ExitCode = 3   // repository, revision or file does not exist
	ExitAuth        ExitCode = 4   // private or gated: token missing or not accepted
	ExitVerify      ExitCode = 5   // received bytes do not match the published hash
	ExitNetwork     ExitCode = 6   // source unreachable or failing after retries
	ExitDisk        ExitCode = 7   // not enough space, or a write failed
	ExitInterrupted ExitCode = 130 // interrupted by the user; progress is kept
)

// Fix is a concrete next step: a command to run, or a note.
type Fix struct {
	Cmd  string `json:"cmd,omitempty"`
	Note string `json:"note,omitempty"`
}

// Error is what every lmget failure turns into: what happened, why, and what
// to do next. It is rendered by the CLI and serialized in --json mode.
type Error struct {
	Code  ExitCode `json:"code"`
	Kind  string   `json:"kind"`
	Msg   string   `json:"message"`
	Why   string   `json:"why,omitempty"`
	Fixes []Fix    `json:"fixes,omitempty"`
	Err   error    `json:"-"`
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Msg + ": " + e.Err.Error()
	}
	return e.Msg
}

func (e *Error) Unwrap() error { return e.Err }

func newErr(code ExitCode, kind, msg string) *Error {
	return &Error{Code: code, Kind: kind, Msg: msg}
}

func (e *Error) why(format string, a ...any) *Error {
	e.Why = fmt.Sprintf(format, a...)
	return e
}

func (e *Error) fix(cmd, note string) *Error {
	e.Fixes = append(e.Fixes, Fix{Cmd: cmd, Note: note})
	return e
}

func (e *Error) wrap(err error) *Error {
	e.Err = err
	return e
}

// AsError converts any error into an *Error with an exit code.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if errors.Is(err, context.Canceled) {
		return newErr(ExitInterrupted, "interrupted", "verified progress is kept on disk").
			fix("", "run the same command again to resume")
	}
	return &Error{Code: ExitFailure, Kind: "failure", Msg: err.Error(), Err: err}
}
