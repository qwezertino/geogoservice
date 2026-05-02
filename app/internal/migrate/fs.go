package migrate

import "io/fs"

// MigrationsFS carries the embedded filesystem and the subdirectory that
// contains the *.up.sql / *.down.sql files.
// Declare the embed in the package that imports migrate (e.g. main) and pass
// the value here, because //go:embed directives only work in the package where
// they are written.
type MigrationsFS struct {
	FS  fs.FS
	Dir string
}
