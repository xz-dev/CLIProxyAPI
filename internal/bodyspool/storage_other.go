//go:build !linux

package bodyspool

import (
	"errors"
	"os"
)

var errUnsupported = errors.New("bodyspool: anonymous O_TMPFILE spools need Linux; keep body-spool disabled on this platform")

func openTmpFile(string) (*os.File, error)          { return nil, errUnsupported }
func syncDrop(*os.File, int64, int64) error         { return errUnsupported }
func dropRange(*os.File, int64, int64) error        { return errUnsupported }
func checkFloor(string, int64, int64, string) error { return errUnsupported }
func CheckDir(string) error                         { return errUnsupported }
