package karakeep

import "errors"

func asPermanent(err error, target *interface{ Permanent() bool }) bool {
	return errors.As(err, target) && (*target).Permanent()
}
