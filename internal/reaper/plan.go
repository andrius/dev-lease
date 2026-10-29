package reaper

import (
	"slices"
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
	// removes, by name; an image with several tags is listed by the tags
	// the removal deletes (ImageScope.Step).
	Containers []string
	Networks   []string
	Volumes    []string
	Images     []string
	// Kept is every object that stays because of a keep label or a kept
	// container's use, by name, and the tags of a kept service's images. Images a container merely runs from without
	// being removable are not kept: they just stay, unlisted.
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

// ImageScope is what the image rule needs to know about one project at
// one moment: its services (ServicesOf), the services of its kept
// containers, and the image ids its containers run from.
type ImageScope struct {
	Project  string
	Services []string
	// KeptServices are the service labels of the kept containers.
	KeptServices map[string]bool
	// Used and KeptImages are the image ids the non-kept and the kept
	// containers run from.
	Used, KeptImages map[string]bool
}

// NewImageScope builds the scope from one container listing and the
// services the lease recorded.
func NewImageScope(project string, conts []docker.Object, recorded []string) ImageScope {
	sc := ImageScope{Project: project, Services: ServicesOf(conts, recorded),
		KeptServices: map[string]bool{}, Used: map[string]bool{}}
	_, _, sc.KeptImages = keptUses(conts)
	for _, c := range conts {
		if docker.Kept(c) {
			if s := c.Labels[docker.ServiceLabel]; s != "" {
				sc.KeptServices[s] = true
			}
			continue
		}
		if c.ImageID != "" {
			sc.Used[c.ImageID] = true
		}
	}
	return sc
}

// ImageRef is one delete of a removal, DELETE /images/<Ref>, reported
// under Name. Expect is set for a delete by tag: the id of the image the
// tag must still name right before the delete, since a tag can move to
// another image after the listing. It is empty for a delete by id.
type ImageRef struct{ Ref, Name, Expect string }

// ImageStep is what the project's removal does with one image.
type ImageStep struct {
	// Remove is what the removal deletes, in order.
	Remove []ImageRef
	// Kept names what stays because a kept service built it or a kept
	// container runs from it.
	Kept []string
	// Reason says why the kept names stay, or why the whole image stays
	// when it is the project's but not removed. A step with no Remove,
	// no Kept and no Reason means the image is no part of the project.
	Reason string
}

// Step is the image rule, the one the sweep's removal, RemoveNow,
// PlanFor, the detail page, status and the presence check share.
//
// An image with one tag (or none) is deleted by id, which names exactly
// the listed image. Docker refuses to delete an image with several tags
// by id, so such an image loses the stack's own tags one by one, each
// checked right before its delete to still name the listed image (a
// build can move a tag in between); any other tag keeps the image.
//
// An image carrying the project's label is the project's when a
// container of the project ran from it or one of its tags has the
// <project>-<service> shape for the image's own service label.
//
// An image carrying no project label (Compose v2 builds images without
// labels) is the project's through its tags: each tag whose repository is
// exactly <project>-<service> for one of the project's services.
// Identical builds share one image id, across services and projects;
// deleting a tag only untags it while other tags remain, and deletes the
// image with its last tag unless a container uses it. The tags of a kept service stay, and so
// does the whole image when a kept container runs from it and deleting
// the stack's tags would take its last tag.
//
// An image carrying another project's label is not this project's, but a
// tag of the shape <project>-<service> for a service of this project is
// this project's own tag on it, because identical builds share one image
// id across projects: only that tag goes, and the image is deleted only
// with its last tag.
//
// A pulled image such as postgres:16-alpine is never deleted, whether
// or not a container ran from it.
func (sc ImageScope) Step(i docker.Object) ImageStep {
	switch i.Labels[docker.ProjectLabel] {
	case sc.Project:
		switch {
		case sc.KeptImages[i.ID]:
			return ImageStep{Kept: []string{i.Name}, Reason: "a kept container of the project runs from it"}
		case !sc.Used[i.ID] && !docker.ImageTagMatches(sc.Project, i):
			if i.Untagged {
				return ImageStep{Reason: "untagged leftover of an earlier build"}
			}
			return ImageStep{Reason: "no container of the project runs from it and its tags do not carry the project's service name"}
		}
		st := sc.ownTags(i)
		if len(i.RepoTags) <= 1 && len(st.Kept) == 0 {
			return ImageStep{Remove: []ImageRef{{Ref: i.ID, Name: i.Name}}}
		}
		switch {
		case len(st.Kept) > 0:
			st.Reason = "built for a kept service of the project"
		case len(st.Remove) == 0:
			st.Reason = "its tags are not the stack's own <project>-<service> names"
		}
		return st
	case "":
		st := sc.ownTags(i)
		if len(st.Remove) == 1 && len(i.RepoTags) == 1 {
			// The stack's tag is the image's only one: deleted by id, which
			// names exactly the listed image.
			st.Remove[0] = ImageRef{Ref: i.ID, Name: st.Remove[0].Name}
		}
		switch {
		case len(st.Kept) > 0:
			st.Reason = "built for a kept service of the project"
		case len(st.Remove) > 0:
			if sc.KeptImages[i.ID] && len(st.Remove) == len(i.RepoTags) {
				// Deleting the stack's tags would take the image's last
				// tag, which Docker refuses while the kept container runs
				// from it: the image stays whole.
				kept := make([]string, 0, len(st.Remove))
				for _, r := range st.Remove {
					kept = append(kept, r.Name)
				}
				return ImageStep{Kept: kept, Reason: "a kept container of the project runs from it"}
			}
		case sc.KeptImages[i.ID]:
			return ImageStep{Kept: []string{i.Name}, Reason: "a kept container of the project runs from it"}
		case sc.Used[i.ID]:
			return ImageStep{Reason: "not built for the project: no tag <project>-<service>"}
		}
		return st
	default:
		// An image labelled for another project: not the project's own
		// image, but a tag of the shape <project>-<service> for a
		// service of this project is this project's own tag on it. Only
		// that tag goes; the image stays with the other project's tags.
		st := sc.sharedTags(i)
		if len(st.Remove) == 0 && len(st.Kept) == 0 {
			return ImageStep{} // no tag of the project's on it
		}
		if len(st.Remove) == 1 && len(i.RepoTags) == 1 {
			// The stack's tag is the image's only one: deleted by id,
			// which names exactly the listed image, so the image goes
			// with its last tag.
			st.Remove[0] = ImageRef{Ref: i.ID, Name: st.Remove[0].Name}
		}
		switch {
		case len(st.Kept) > 0:
			st.Reason = "built for a kept service of the project"
		case sc.KeptImages[i.ID] && len(st.Remove) == len(i.RepoTags):
			// Deleting the stack's tags would take the image's last
			// tag, which Docker refuses while the kept container runs
			// from it: the image stays whole.
			kept := make([]string, 0, len(st.Remove))
			for _, r := range st.Remove {
				kept = append(kept, r.Name)
			}
			return ImageStep{Kept: kept, Reason: "a kept container of the project runs from it"}
		}
		return st
	}
}

// ownTags splits the image's tags that are the stack's own, repository
// exactly <project>-<service> for one of the project's services (or the
// image's own service label): a kept service's tags are Kept, every other
// one is a delete by tag that expects the tag to still name this image.
func (sc ImageScope) ownTags(i docker.Object) ImageStep {
	svcs := sc.Services
	if s := i.Labels[docker.ServiceLabel]; s != "" {
		svcs = append(slices.Clone(svcs), s)
	}
	return sc.tagsOf(i, svcs)
}

// sharedTags splits the stack's own tags off an image labelled for
// another project: the image's own service label belongs to that
// project, so only the project's own services name its tags.
func (sc ImageScope) sharedTags(i docker.Object) ImageStep {
	return sc.tagsOf(i, sc.Services)
}

// tagsOf splits the image's tags that are the stack's own, repository
// exactly <project>-<service> for one of the given services: a kept
// service's tags are Kept, every other one is a delete by tag that
// expects the tag to still name this image.
func (sc ImageScope) tagsOf(i docker.Object, svcs []string) ImageStep {
	var st ImageStep
	for _, tag := range i.RepoTags {
		svc, ok := docker.TagService(sc.Project, svcs, tag)
		switch {
		case !ok:
		case sc.KeptServices[svc]:
			st.Kept = append(st.Kept, tag)
		default:
			st.Remove = append(st.Remove, ImageRef{Ref: tag, Name: tag, Expect: i.ID})
		}
	}
	return st
}

// PlanFor computes the plan from listings taken at one moment, applying
// the same rules the engine does: keptUses for what kept containers use,
// docker.Kept for keep labels and ImageScope.Step for images. Nothing
// runs against the daemon here: the dialogs promise exactly what a stop
// and a removal on these listings would do, no more and no less.
func PlanFor(project string, services []string, conts, nets, vols, imgs []docker.Object) Plan {
	var p Plan
	keptNets, keptVols, _ := keptUses(conts)
	for _, c := range conts {
		if docker.Kept(c) {
			p.Kept = append(p.Kept, c.Name)
			continue
		}
		p.Containers = append(p.Containers, c.Name)
		if c.Running {
			p.Stop = append(p.Stop, c.Name)
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
	scope := NewImageScope(project, conts, services)
	for _, i := range imgs {
		step := scope.Step(i)
		p.Kept = append(p.Kept, step.Kept...)
		for _, r := range step.Remove {
			p.Images = append(p.Images, r.Name)
		}
	}
	for _, s := range [][]string{p.Stop, p.Containers, p.Networks, p.Volumes, p.Images, p.Kept} {
		sort.Strings(s)
	}
	return p
}
