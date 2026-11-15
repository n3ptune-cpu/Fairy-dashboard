//go:build windows

package statefile

import "io/fs"

// fileIDOf: nothing. os.Stat does not give out the number of a file on Windows, so a view there tells two
// files apart by their time and size alone, as before; a write of this process is read in any case (Refresh).
func fileIDOf(fs.FileInfo) fileID { return fileID{} }
