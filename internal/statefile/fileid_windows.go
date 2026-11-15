//go:build windows

package statefile

import "io/fs"

// fileIDOf: nothing. os.Stat does not give out the number of a file on Windows, so a look there tells two
// files apart by their time and size alone. A write of this process is read in any case (Refresh), and a file
// the view has not known for long is read at every look (YoungFor).
func fileIDOf(fs.FileInfo) fileID { return fileID{} }
