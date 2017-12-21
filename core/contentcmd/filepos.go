package contentcmd

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/jmigpin/editor/core/cmdutil"
	"github.com/jmigpin/editor/ui/tautil"
)

const FilenameStopRunes = "\"'`&=:<>[]"

// Opens filename.
// Detects compiler errors format <string(:int)?(:int?)>, and goes to line/column.
func filePos(erow cmdutil.ERower) bool {
	ta := erow.Row().TextArea

	var str string
	if ta.SelectionOn() {
		a, b := tautil.SelectionStringIndexes(ta)
		str = ta.Str()[a:b]
	} else {
		isStop := StopOnSpaceAndRunesFn(FilenameStopRunes)
		l, r := expandLeftRightStop(ta.Str(), ta.CursorIndex(), isStop)
		str = ta.Str()[l:r]

		// line
		if r < len(ta.Str()) && ta.Str()[r] == ':' {
			r2 := expandRightStop(ta.Str(), r+1, NotStop(unicode.IsNumber))
			str = ta.Str()[l:r2]

			// column
			if r2 < len(ta.Str()) && ta.Str()[r2] == ':' {
				r3 := expandRightStop(ta.Str(), r2+1, NotStop(unicode.IsNumber))
				str = ta.Str()[l:r3]
			}
		}

	}

	a := strings.Split(str, ":")

	// filename
	if len(a) == 0 {
		return false
	}
	if a[0] == "" {
		return false
	}
	filename, fi, ok := findFileinfo(erow, a[0])
	if !ok || fi.IsDir() {
		return false
	}

	// line and column
	line := 0
	column := 0
	if len(a) > 1 {
		// line
		v, err := strconv.ParseUint(a[1], 10, 64)
		if err == nil {
			line = int(v)
		}
		// column
		if len(a) > 2 {
			v, err := strconv.ParseUint(a[2], 10, 64)
			if err == nil {
				column = int(v)
			}
		}
	}

	// erow
	ed := erow.Ed()
	erow2, ok := ed.FindERower(filename)
	if !ok {
		col, nextRow := ed.GoodColumnRowPlace()
		erow2 = ed.NewERowerBeforeRow(filename, col, nextRow)
		err := erow2.LoadContentClear()
		if err != nil {
			ed.Error(err)
			return true
		}
	}

	if line == 0 && column == 0 {
		erow2.Flash()
	} else {
		cmdutil.GotoLineColumnInTextArea(erow2.Row(), line, column)
	}

	return true
}
