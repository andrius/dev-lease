package reaper

import (
	"sort"

	"github.com/andrius/dev-lease/internal/docker"
)

// Plan is what one project's stop and removal would do on one set of
// daemon listings: the containers the stop stops, the objects the
// removal removes, and every object that stays because of a keep label
// or a kept container's use.
type Plan struct {
	// Stop is the running non-kept containers the stop stops.
	Stop []string
	// Containers, Networks, Volumes and Images are what the removal
	// removes, by name.
	Containers []string
	Networks   []string
	Volumes    []string
	Images     []string
	// Kept is every object that stays because of a keep label or a kept
	// container's use, by name. Images a container merely runs from
	// without being removable are not kept: they just stay, unlisted.
	Kept []string
}

// ServicesOf returns the project's services for the image rule: the
// sorted unique service labels of the project's containers, or, when no
// container is left, the services the sweep recorded in the lease. A
// container without a service label contributes nothing.
func ServicesOf(conts []docker.Object, recorded []string) []string {
	set := map[string]bool{}
	for _, c := range conts {
		if s := c.Labels[docker.ServiceLabel]; s != "" {
			set[s] = true
		}
	}
	if len(set) == 0 {
		for _, s := range recorded {
			if s != "" {
				set[s] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// ImageRemovable reports whether the project's removal removes the image:
// the one rule the sweep's removal, RemoveNow, PlanFor and the detail
// page share. An image carrying the project's label is removable when a
// container of the project ran from it (the ids collected in the pass,
// `used`) or one of its tags has the <project>-<service> shape for the
// image's own service label. An image carrying no project label at all
// (Compose v2 builds its images without labels) is removable only through
// its tags: a repository exactly <project>-<service> for one of the
// project's services, where the services are the service labels of the
// project's containers in the current listing or, when no container is
// left, the services recorded in the lease. A pulled image such as
// postgres:16-alpine never matches, whether or not a container ran from
// it. An image carrying another project's label never matches, whatever
// its tags. An unlabelled image alone never makes the project present:
// presence finds projects by label, and an unlabelled image carries none.
func ImageRemovable(i docker.Object, project string, services []string, used map[string]bool) bool {
	switch p := i.Labels[docker.ProjectLabel]; p {
	case project:
		return used[i.ID] || docker.ImageTagMatches(project, i)
	case "":
		for _, s := range services {
			if docker.TagMatchesService(project, s, i.RepoTags) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// PlanFor computes the plan from listings taken at one moment, applying
// the same rules the engine does: keptUses for what kept containers use,
// docker.Kept for keep labels and ImageRemovable for images. Nothing
// runs against the daemon here: the dialogs promise exactly what a stop
// and a removal on these listings would do, no more and no less.
func PlanFor(project string, services []string, conts, nets, vols, imgs []docker.Object) Plan {
	var p Plan
	keptNets, keptVols, keptImgs := keptUses(conts)
	usedImgs := map[string]bool{}
	for _, c := range conts {
		if docker.Kept(c) {
			p.Kept = append(p.Kept, c.Name)
			continue
		}
		p.Containers = append(p.Containers, c.Name)
		if c.Running {
			p.Stop = append(p.Stop, c.Name)
		}
		if c.ImageID != "" {
			usedImgs[c.ImageID] = true
		}
	}
	for _, n := range nets {
		if keptNets[n.Name] {
			p.Kept = append(p.Kept, n.Name)
		} else {
			p.Networks = append(p.Networks, n.Name)
		}
	}
	for _, v := range vols {
		if docker.Kept(v) || keptVols[v.Name] {
			p.Kept = append(p.Kept, v.Name)
		} else {
			p.Volumes = append(p.Volumes, v.Name)
		}
	}
	for _, i := range imgs {
		// Images of another project and unlabelled images the rule does
		// not match are not part of this project's plan at all.
		if p := i.Labels[docker.ProjectLabel]; p != "" && p != project {
			continue
		}
		if keptImgs[i.ID] {
			p.Kept = append(p.Kept, i.Name)
			continue
		}
		if !ImageRemovable(i, project, services, usedImgs) {
			// Pulled images, untagged leftovers and images that only
			// inherited the project label stay, but they are not kept.
			continue
		}
		p.Images = append(p.Images, i.Name)
	}
	for _, s := range [][]string{p.Stop, p.Containers, p.Networks, p.Volumes, p.Images, p.Kept} {
		sort.Strings(s)
	}
	return p
}
