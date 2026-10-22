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

// PlanFor computes the plan from listings taken at one moment, applying
// the same rules the engine does: keptUses for what kept containers use,
// docker.Kept for keep labels and removableImage for images. Nothing
// runs against the daemon here: the dialogs promise exactly what a stop
// and a removal on these listings would do, no more and no less.
func PlanFor(project string, conts, nets, vols, imgs []docker.Object) Plan {
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
		if keptImgs[i.ID] {
			p.Kept = append(p.Kept, i.Name)
			continue
		}
		if !removableImage(i, project, usedImgs) {
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
