package dest

import (
	"context"
	"errors"
	"time"
)

// Checker is implemented by a destination that can test its connection with
// one cheap authenticated read that changes nothing upstream.
type Checker interface {
	Check(ctx context.Context) error
}

// CheckTimeout bounds one connection test, so a save never waits long.
const CheckTimeout = 10 * time.Second

// ErrUnexpectedReply marks a reply that came back 2xx but is not what the
// service sends. Wrap it with UnexpectedReply.
var ErrUnexpectedReply = errors.New("unexpected reply")

// ErrNoCheck means the kind has no connection test.
var ErrNoCheck = errors.New("this kind of connection has no test")

type unexpectedReply struct{ msg string }

func (u unexpectedReply) Error() string        { return u.msg }
func (u unexpectedReply) Is(target error) bool { return target == ErrUnexpectedReply }

// UnexpectedReply returns an error whose text is msg and that matches ErrUnexpectedReply.
func UnexpectedReply(msg string) error { return unexpectedReply{msg} }

// CheckFunc returns a function that tests a connection of any kind in
// factories, within CheckTimeout. It never logs or returns the secret.
func CheckFunc(factories map[string]Factory) func(ctx context.Context, kind, baseURL, secret string) error {
	return func(ctx context.Context, kind, baseURL, secret string) error {
		f, ok := factories[kind]
		if !ok {
			return ErrNoCheck
		}
		c, ok := f(baseURL, secret, nil).(Checker)
		if !ok {
			return ErrNoCheck
		}
		ctx, cancel := context.WithTimeout(ctx, CheckTimeout)
		defer cancel()
		return c.Check(ctx)
	}
}
