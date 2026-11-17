package export

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"path"
	"slices"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/slack"
)

// MessageError reports one entry of a day file that could not be used as a
// message. It never contains the entry's content.
type MessageError struct {
	Channel   string // channel name, or the conversation ID for DMs
	ChannelID string
	Day       string // YYYY-MM-DD
	File      string // path inside the export, e.g. "general/2024-03-04.json"
	Index     int    // zero-based position in the day file's array
	Err       error
}

func (e *MessageError) Error() string {
	return fmt.Sprintf("channel %q, file %s, entry %d: %v", e.Channel, e.File, e.Index, e.Err)
}

func (e *MessageError) Unwrap() error { return e.Err }

// Messages streams every message of ch in ascending timestamp order.
//
// Day files are visited in date order and decoded one at a time, entry by
// entry, so memory use is bounded by the largest single day of the channel.
// Within a day, messages are sorted by TS compared numerically as (seconds,
// microseconds); entries with equal TS keep their file order. Ordering across
// days relies on Slack's layout, in which a day file only holds messages of
// that day.
//
// Errors are yielded as values with a zero Message, and iteration continues
// after each of them unless the caller stops:
//
//   - A *MessageError for every entry that is not a usable message (not an
//     object, wrong field types, missing or invalid ts). These are yielded
//     before the valid messages of the same day file, since an entry without
//     a usable ts has no position.
//   - A *fail.Error (export.day_unreadable) when a day file cannot be read or
//     its JSON is broken. Messages decoded before the break are still
//     yielded first; then iteration moves on to the next day.
//   - A *fail.Error when the channel's folder cannot be listed, which ends
//     the sequence.
func (e *Export) Messages(ch slack.Channel) iter.Seq2[slack.Message, error] {
	return func(yield func(slack.Message, error) bool) {
		days, err := e.Days(ch)
		if err != nil {
			yield(slack.Message{}, err)
			return
		}
		for _, day := range days {
			msgs, bad, err := e.readDay(ch, day)
			for _, b := range bad {
				if !yield(slack.Message{}, b) {
					return
				}
			}
			for _, m := range msgs {
				if !yield(m, nil) {
					return
				}
			}
			if err != nil && !yield(slack.Message{}, err) {
				return
			}
		}
	}
}

// DayMessages returns the messages of one day file of ch, sorted like
// Messages sorts them. day is a key returned by Days.
//
// If some entries are unusable, DayMessages returns the messages that did
// decode together with a non-nil error joining one *MessageError per rejected
// entry (and a *fail.Error if the file itself was unreadable or broke off
// midway). Use errors.As to inspect it; callers that want all-or-nothing
// semantics should discard the messages when err is non-nil.
func (e *Export) DayMessages(ch slack.Channel, day string) ([]slack.Message, error) {
	msgs, bad, err := e.readDay(ch, day)
	if len(bad) == 0 {
		return msgs, err
	}
	errs := make([]error, 0, len(bad)+1)
	for _, b := range bad {
		errs = append(errs, b)
	}
	if err != nil {
		errs = append(errs, err)
	}
	return msgs, errors.Join(errs...)
}

// readDay decodes one day file. bad holds per-entry problems; err is set
// when the file as a whole could not be read to its end.
func (e *Export) readDay(ch slack.Channel, day string) (msgs []slack.Message, bad []*MessageError, err error) {
	folder, err := channelFolder(ch)
	if err != nil {
		return nil, nil, err
	}
	if !isDayKey(day) {
		return nil, nil, fail.New("export.bad_day",
			fmt.Sprintf("%q is not a day of channel %q", day, ch.Label()),
			"Day keys have the form YYYY-MM-DD.",
			"Use a key returned by the export's day listing.")
	}
	file := path.Join(folder, day+".json")

	err = e.readArray(file, func(i int, raw json.RawMessage) error {
		var m slack.Message
		err := unmarshalObject(raw, &m)
		if err == nil {
			switch _, _, ok := slack.ParseTS(m.TS); {
			case m.TS == "":
				err = errors.New("message has no ts")
			case !ok:
				err = errors.New(`ts is not a "seconds.microseconds" timestamp`)
			}
		}
		if err != nil {
			bad = append(bad, &MessageError{
				Channel: ch.Label(), ChannelID: ch.ID, Day: day, File: file, Index: i, Err: err,
			})
			return nil //nolint:nilerr // recorded above; one bad entry must not stop the day
		}
		msgs = append(msgs, m)
		return nil
	})
	slices.SortStableFunc(msgs, func(a, b slack.Message) int { return slack.CompareTS(a.TS, b.TS) })
	if err != nil {
		why := ""
		if errors.Is(err, fs.ErrNotExist) {
			why = "The export has no file for that day."
		}
		err = fail.Wrap(err, "export.day_unreadable",
			fmt.Sprintf("Taut cannot read %s in the export at %s", file, e.label), why,
			"Messages from this day file may be incomplete. "+nextReexport)
	}
	return msgs, bad, err
}
