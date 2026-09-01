package main

import "fmt"

// versionString names the build. On a rolling channel the version is "edge", which identifies a
// moving target -- the commit is what makes a bug report answerable.
func versionString() string {
	if commit == "" {
		return version
	}
	return fmt.Sprintf("%s (%s)", version, commit)
}
