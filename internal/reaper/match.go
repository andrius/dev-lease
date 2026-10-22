package reaper

import (
	"path"
	"regexp"
)

// composeProject matches the names Compose can create. The daemon holds
// whatever labels it was given (a hand-set label can name anything), but
// a project outside this rule can never have been created by Compose, so
// dev-lease can never manage it; refusing it early keeps shell
// metacharacters out of later handling.
var composeProject = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Managed reports whether a Compose project name is managed: it matches at
// least one glob pattern and is not the dev-lease project itself.
func Managed(project string, cfg Config) bool {
	if project == "" || project == cfg.Own || !composeProject.MatchString(project) {
		return false
	}
	for _, p := range cfg.Patterns {
		if ok, err := path.Match(p, project); err == nil && ok {
			return true
		}
	}
	return false
}
