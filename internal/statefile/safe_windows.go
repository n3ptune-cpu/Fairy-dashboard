package statefile

import "os"

// openNoFollow: Windows has no O_NOFOLLOW; what is at path is looked at first (a link, a folder and a pipe are
// refused by name there), and OpenRegular looks at the opened file again.
func openNoFollow(path string) (*os.File, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, &NotAFileError{Path: path, What: what(st.Mode())}
	}
	return os.Open(path)
}
