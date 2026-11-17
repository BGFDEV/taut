package graph

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/bgfdev/taut/internal/fail"
)

// ErrNotFound matches (with errors.Is) any error caused by Graph answering
// 404 Not Found.
var ErrNotFound = errors.New("graph: resource not found")

// IsNotFound reports whether err was caused by a 404 from Graph.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// ErrConflict matches (with errors.Is) any error caused by Graph answering
// 409 Conflict. For an import it means a message with the same
// createdDateTime already exists in the channel.
var ErrConflict = errors.New("graph: conflict")

// IsConflict reports whether err was caused by a 409 from Graph.
func IsConflict(err error) bool { return errors.Is(err, ErrConflict) }

// Sentinels for the migration-mode and channel outcomes callers branch on.
// Each matches with errors.Is; the Is… helpers are shorthand.
var (
	// ErrAlreadyMigrating: startMigration on a channel that is already in
	// migration mode.
	ErrAlreadyMigrating = errors.New("graph: channel is already in migration mode")
	// ErrNotInMigration: an import into, or completeMigration of, a channel
	// (or completeMigration of a team) that is not in migration mode.
	ErrNotInMigration = errors.New("graph: channel is not in migration mode")
	// ErrChannelsMigrating: completeMigration of a team that still has a
	// channel in migration mode.
	ErrChannelsMigrating = errors.New("graph: team has channels in migration mode")
	// ErrChannelExists: creating a channel whose name is taken in the team.
	ErrChannelExists = errors.New("graph: channel name already exists")
	// ErrNotTeamMember: adding someone to a private channel (or naming them
	// its owner) who is not a member of the team.
	ErrNotTeamMember = errors.New("graph: user is not a member of the team")
	// ErrChatHistoryStart: creating a chat whose members carry a
	// visibleHistoryStartDateTime, refused because of that property.
	ErrChatHistoryStart = errors.New("graph: chat members cannot carry a history start time at creation")
)

// ErrChatRefused is Graph refusing to create a chat, or to start its
// migration mode, for a reason that belongs to that chat or its people.
var ErrChatRefused = errors.New("graph: the chat was refused")

// IsChatRefused reports whether err is Graph refusing one chat for a
// reason of its own (see ErrChatRefused). The app's other chats are not
// affected.
func IsChatRefused(err error) bool { return errors.Is(err, ErrChatRefused) }

// ErrMentionInvalid is an imported message refused because of one of its
// @mentions.
var ErrMentionInvalid = errors.New("graph: a mention was refused")

// IsMentionInvalid reports whether err is Graph refusing an imported
// message because of someone it mentions.
func IsMentionInvalid(err error) bool { return errors.Is(err, ErrMentionInvalid) }

// ErrReactionInvalid is an imported message refused because of the
// reactions it carries.
var ErrReactionInvalid = errors.New("graph: the reactions were refused")

// IsReactionInvalid reports whether err is Graph refusing an imported
// message because of its reactions. The message can be sent without them.
func IsReactionInvalid(err error) bool { return errors.Is(err, ErrReactionInvalid) }

// IsChatHistoryStart reports whether err is Graph refusing to create a chat
// because its members carry visibleHistoryStartDateTime. The chat can be
// created without it.
func IsChatHistoryStart(err error) bool { return errors.Is(err, ErrChatHistoryStart) }

// IsNotTeamMember reports whether err is Graph refusing a private-channel
// member because the user is not a member of the team.
func IsNotTeamMember(err error) bool { return errors.Is(err, ErrNotTeamMember) }

// IsAlreadyMigrating reports whether err is Graph refusing startMigration
// because the channel is already in migration mode. After a startMigration
// whose response was lost, a retry fails this way and can be treated as
// success, provided this app started the session (an import will say so).
func IsAlreadyMigrating(err error) bool { return errors.Is(err, ErrAlreadyMigrating) }

// IsNotInMigration reports whether err is Graph refusing an import or a
// completeMigration because the channel is not in migration mode.
func IsNotInMigration(err error) bool { return errors.Is(err, ErrNotInMigration) }

// IsChannelsMigrating reports whether err is Graph refusing to complete a
// team's migration because one of its channels has not completed its own.
func IsChannelsMigrating(err error) bool { return errors.Is(err, ErrChannelsMigrating) }

// IsChannelExists reports whether err is Graph refusing to create a channel
// because one with that name already exists in the team.
func IsChannelExists(err error) bool { return errors.Is(err, ErrChannelExists) }

// APIError is an error response from Microsoft Graph, or a transport
// failure talking to it. It is carried inside the *fail.Error the client
// returns; reach it with errors.As.
type APIError struct {
	// Method and Path identify the request. The query string is omitted.
	Method string
	Path   string
	// Status is the HTTP status code; zero for transport failures.
	Status int
	// Code and Message come from Graph's error body.
	Code    string
	Message string
	// RequestID and Date come from innerError (or the request-id header);
	// Microsoft support asks for both.
	RequestID string
	Date      string
	// RetryAfter is the server's Retry-After, if it sent one.
	RetryAfter time.Duration
	// Attempts is how many times the request was sent.
	Attempts int
	// Err is the transport error, if any.
	Err error
	// kind is the sentinel (ErrAlreadyMigrating, …) this response was
	// classified as, if any.
	kind error
}

func (e *APIError) Error() string {
	s := e.Method + " " + e.Path + ": "
	switch {
	case e.Status == 0 && e.Err != nil:
		s += e.Err.Error()
	case e.Code != "":
		s += fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message)
	default:
		s += fmt.Sprintf("%d %s", e.Status, http.StatusText(e.Status))
	}
	if e.RequestID != "" {
		s += " (request-id " + e.RequestID + ")"
	}
	return s
}

// Unwrap returns the transport error, if any.
func (e *APIError) Unwrap() error { return e.Err }

// Is makes a 404 match ErrNotFound, a 409 match ErrConflict, and a
// classified response match its sentinel.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.Status == http.StatusNotFound
	case ErrConflict:
		return e.Status == http.StatusConflict
	}
	return e.kind != nil && target == e.kind
}

// ref renders the request, status, Graph code and request-id for the Why
// line of an error. It never contains request or response bodies.
func (e *APIError) ref() string {
	ref := fmt.Sprintf(" [%s %s → HTTP %d", e.Method, e.Path, e.Status)
	if e.Code != "" {
		ref += " " + e.Code
	}
	if e.RequestID != "" {
		ref += ", request-id " + e.RequestID
	}
	return ref + "]"
}

// detail is Graph's message, or the status text when there is none.
func (e *APIError) detail() string {
	if e.Message != "" {
		return e.Message
	}
	return http.StatusText(e.Status)
}

// parseAPIError builds an APIError from a non-2xx response body.
func parseAPIError(method, path string, resp *http.Response, body []byte) *APIError {
	e := &APIError{Method: method, Path: path, Status: resp.StatusCode}
	var raw struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Inner   struct {
				RequestID string `json:"request-id"`
				Date      string `json:"date"`
			} `json:"innerError"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &raw) == nil {
		e.Code, e.Message = raw.Error.Code, raw.Error.Message
		e.RequestID, e.Date = raw.Error.Inner.RequestID, raw.Error.Inner.Date
	}
	if e.RequestID == "" {
		e.RequestID = resp.Header.Get("request-id")
	}
	return e
}

// failure converts an APIError into the operator-facing error. permission
// is the application permission the endpoint needs, if known. giveUp
// explains why retrying stopped, for retryable statuses.
func failure(e *APIError, permission, giveUp string) *fail.Error {
	detail, ref := e.detail(), e.ref()
	if giveUp != "" {
		giveUp = " " + giveUp
	}

	switch {
	case e.Status == 0:
		return fail.Wrap(e, "graph.network",
			"Microsoft Graph could not be reached",
			fmt.Sprintf("%s %s failed after %d attempt(s): %v.%s", e.Method, e.Path, e.Attempts, e.Err, giveUp),
			"Check network access and proxy settings, confirm the cloud setting matches the tenant, then run `taut doctor`.")
	case e.Status == http.StatusUnauthorized:
		return fail.Wrap(e, "graph.unauthorized",
			"Microsoft Graph rejected the access token",
			detail+ref,
			"Run `taut auth` to re-check credentials. If the tenant is in a sovereign cloud, set the matching cloud "+
				"(usgovhigh, dod, china) so the token is issued for the right Graph endpoint.")
	case e.Status == http.StatusForbidden:
		why := detail + ref
		next := "Grant the missing application permission to the app registration with admin consent " +
			"(Entra admin center → App registrations → API permissions), then run `taut auth` to confirm."
		if permission != "" {
			why = "The app is not allowed to call this endpoint; it needs the application permission " + permission + ". " + why
			next = "Add the Microsoft Graph application permission " + permission +
				" to the app registration and grant admin consent (Entra admin center → App registrations → API permissions), " +
				"then run `taut auth` to confirm."
		}
		return fail.Wrap(e, "graph.forbidden",
			"Microsoft Graph denied the request", why, next).WithExit(fail.ExitPermission)
	case e.Status == http.StatusNotFound:
		return fail.Wrap(e, "graph.not_found",
			"Microsoft Graph could not find the requested item",
			detail+ref,
			"Check the ID or name. If it was created moments ago, wait a few seconds and try again.")
	case e.Status == http.StatusTooManyRequests:
		return fail.Wrap(e, "graph.throttled",
			"Microsoft Graph is throttling this app",
			fmt.Sprintf("Still throttled after %d attempt(s).%s%s", e.Attempts, giveUp, ref),
			"Wait a few minutes and run the command again. If other tools use the same app registration, pause them or lower Taut's rate limits.")
	case e.Status >= 500:
		return fail.Wrap(e, "graph.unavailable",
			"Microsoft Graph is temporarily unavailable",
			fmt.Sprintf("%s after %d attempt(s).%s%s", detail, e.Attempts, giveUp, ref),
			"Wait a few minutes and run the command again; it is safe to repeat. Check https://status.cloud.microsoft if it persists.")
	case e.Status == http.StatusConflict:
		return fail.Wrap(e, "graph.conflict",
			"Microsoft Graph reported a conflict with existing data",
			detail+ref,
			"Check whether the item already exists before creating it again; if a previous run was interrupted, resume it with `taut migrate`.")
	case e.Status == http.StatusRequestEntityTooLarge:
		return fail.Wrap(e, "graph.payload_too_large",
			"Microsoft Graph rejected a request as too large",
			detail+ref,
			"Reduce the size of the item being sent and try again.")
	case e.Status == http.StatusBadRequest:
		return fail.Wrap(e, "graph.bad_request",
			"Microsoft Graph did not accept the request",
			detail+ref,
			"This is likely a bug in Taut. Re-run with --verbose and report the request-id shown above.")
	default:
		return fail.Wrap(e, "graph.request_failed",
			"Microsoft Graph returned an unexpected error",
			detail+ref,
			"Re-run with --verbose. If it persists, report the request-id shown above.")
	}
}
