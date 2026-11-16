package interpreterauthority

import (
	"path/filepath"
	"strings"
)

// LinuxInvocation verifies the bounded search closure before returning a
// command prefix. It never runs the loader or claims process admission.
func (r *RegisteredCatalog) LinuxInvocation() ([]string, error) {
	doc := r.document
	if doc.Binding.Platform != "linux" || doc.Linux == nil {
		return nil, refusal("authority_linux_entry_required")
	}
	objects := map[string]Entry{}
	for _, e := range doc.Prefix.Entries {
		objects[filepath.Join(doc.Prefix.Root, e.Path)] = e
	}
	for _, e := range doc.External {
		objects[e.Entry.Path] = e.Entry
	}
	loader := doc.Linux.Loader
	directory := doc.Linux.LibraryDirectory
	if objects[loader].Kind != "file" || objects[directory].Kind != "directory" {
		return nil, refusal("authority_linux_loader_missing")
	}
	search := map[string]SearchPath{}
	for _, s := range doc.SearchPaths {
		search[s.Path] = s
	}
	closed := search[directory]
	if closed.State != "closed-directory" || search["/etc/ld.so.preload"].State != "absent" {
		return nil, refusal("authority_linux_search_unbound")
	}
	for _, name := range closed.Members {
		// No nested hwcaps/tls directories or aliases can select an unmodeled
		// object in this sealed, SONAME-keyed dependency directory.
		if objects[filepath.Join(directory, name)].Kind != "file" {
			return nil, refusal("authority_linux_library_member")
		}
	}
	images := map[string]NativeImage{}
	for _, image := range doc.NativeImages {
		images[image.Path] = image
	}
	if images[loader].Format != "elf" {
		return nil, refusal("authority_linux_loader_missing")
	}
	for _, image := range doc.NativeImages {
		if image.Format != "elf" {
			return nil, refusal("authority_native_platform")
		}
		for _, name := range image.Needed {
			if strings.ContainsAny(name, "/\\$") {
				path, err := originNeeded(name, image.Path, doc.Prefix.Root, objects)
				if err != nil || images[path].Format != "elf" {
					return nil, refusal("authority_linux_needed_unsupported")
				}
				continue
			}
			// Every recursively modeled NEEDED has a sealed front-path target.
			// Therefore no missing dependency can reach cache/default search.
			path := filepath.Join(directory, name)
			entry, found := objects[path]
			if !found || entry.Kind != "file" || images[path].Format != "elf" {
				return nil, refusal("authority_linux_needed_unbound")
			}
			observed, native, err := inspectNative(path, doc.Binding.Architecture)
			if err != nil || !native || (observed.soname != "" && observed.soname != name) || (observed.soname == "" && filepath.Base(path) != name) {
				return nil, refusal("authority_linux_needed_soname")
			}
		}
		for _, group := range append(append([]string{}, image.RPath...), image.RunPath...) {
			for _, part := range strings.Split(group, ":") {
				part = strings.ReplaceAll(strings.ReplaceAll(part, "${ORIGIN}", filepath.Dir(image.Path)), "$ORIGIN", filepath.Dir(image.Path))
				if strings.Contains(part, "$") || !filepath.IsAbs(part) {
					return nil, refusal("authority_linux_search_unsupported")
				}
				part = filepath.Clean(part)
				if within(part, doc.Prefix.Root) {
					if objects[part].Kind != "directory" {
						return nil, refusal("authority_linux_search_unbound")
					}
				} else if part != directory && search[part].State != "absent" {
					return nil, refusal("authority_linux_external_rpath")
				}
			}
		}
	}
	return []string{loader, "--inhibit-cache", "--glibc-hwcaps-mask", "", "--library-path", filepath.Join(doc.Prefix.Root, "lib") + ":" + directory, filepath.Join(doc.Prefix.Root, doc.Executable)}, nil
}
