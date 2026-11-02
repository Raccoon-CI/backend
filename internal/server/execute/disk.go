package execute

import (
	"errors"
	"fmt"
	"io"
	"syscall"
)

// DiskFree reports the bytes available on the disk holding the work directory.
func DiskFree(dir string) func() (int64, error) {
	return func() (int64, error) {
		var st syscall.Statfs_t
		if err := syscall.Statfs(dir, &st); err != nil {
			return 0, err
		}
		return int64(st.Bavail) * int64(st.Bsize), nil
	}
}

// defaultMaxSource caps the source package of one run: a repository bigger
// than this fails its run with a clear message instead of filling the disk.
const defaultMaxSource = 2 << 30

// errSourceTooBig is returned by a capped writer.
var errSourceTooBig = errors.New("the repository at this commit is bigger than the limit for a run's source")

// cappedWriter stops a write that would take the total over max.
type cappedWriter struct {
	w   io.Writer
	n   int64
	max int64
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.n+int64(len(p)) > c.max {
		return 0, fmt.Errorf("%w (%d MiB)", errSourceTooBig, c.max>>20)
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// lowOnDisk says whether new runs should wait: the work directory's disk has
// less than MinFreeBytes available. A failure to find out never blocks runs.
func (e *Executor) lowOnDisk() (bool, int64) {
	if e.MinFreeBytes <= 0 || e.Free == nil {
		return false, 0
	}
	free, err := e.Free()
	if err != nil {
		return false, 0
	}
	return free < e.MinFreeBytes, free
}
