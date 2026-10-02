package main

import "fmt"

func versionString() string {
	if commit == "" {
		return version
	}
	return fmt.Sprintf("%s (%s)", version, commit)
}
