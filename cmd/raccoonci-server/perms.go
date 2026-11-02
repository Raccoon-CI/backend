package main

import (
	"log/slog"
	"os"
)

// privateDatabase makes the database files readable by the server's user alone.
// The database holds the hashes of tokens, the payloads of GitHub's deliveries,
// the runs' logs and the (sealed) secrets: other users of the machine have no
// business reading it. SQLite creates its -wal and -shm files with the mode of
// the database file, so the file is made first with the right mode, and files
// left by an older start with a looser one are tightened before the database is
// opened.
func privateDatabase(path string, log *slog.Logger) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm()&0o077 == 0 {
			continue
		}
		if err := os.Chmod(p, 0o600); err != nil {
			return err
		}
		log.Info("the database file was readable by others; now mode 600", "file", p)
	}
	return nil
}

// warnOpenDirectory says so when other users can enter the data directory,
// which also holds what is kept on disk of artifacts and caches. It does not
// change it: the directory is the operator's, and may be shared on purpose.
func warnOpenDirectory(dir string, log *slog.Logger) {
	if info, err := os.Stat(dir); err == nil && info.Mode().Perm()&0o007 != 0 {
		log.Warn("the data directory can be entered by any user of this machine; make it mode 700", "dir", dir, "mode", info.Mode().Perm().String())
	}
}
