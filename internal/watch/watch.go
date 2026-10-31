// Package watch finds what changed under the originals directory. It does no
// I/O beyond reading the tree, so the decisions can be tested on their own.
package watch

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"jellyfinner/internal/queue"
)

var videoExts = []string{
	".avi", ".m2ts", ".m4v", ".mkv", ".mov", ".mp4", ".mpeg", ".mpg", ".ts", ".webm", ".wmv",
}

// File is one video found by a scan.
type File struct {
	Size    int64
	MtimeNS int64
}

// Scan walks root and returns videos by slash-separated relative path. Any
// read error fails the whole scan: a partial listing would look like deletions.
func Scan(root string) (map[string]File, error) {
	files := map[string]File{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !isVideo(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = File{Size: info.Size(), MtimeNS: info.ModTime().UnixNano()}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", root, err)
	}
	return files, nil
}

func skipDir(name string) bool {
	return strings.HasPrefix(name, ".") || name == "@eaDir"
}

// isVideo is an allowlist, so temporary names such as x.mkv.part or x.mkv.!qB
// are ignored until the downloader renames them.
func isVideo(name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}
	return slices.Contains(videoExts, strings.ToLower(filepath.Ext(name)))
}

// Rename is a tracked source found again at a new path.
type Rename struct {
	From *queue.Source
	To   string
}

// Changes is the difference between tracked sources and a scan.
type Changes struct {
	New      []string
	Changed  []string
	Returned []string // reappeared unchanged after going missing
	Renamed  []Rename
	Missing  []*queue.Source
}

// Classify compares tracked sources with a scan. A missing source and a new
// file with the same size and mtime are one rename, unless several candidates
// make that ambiguous, in which case they stay a delete and an add.
func Classify(tracked []*queue.Source, scan map[string]File) Changes {
	var c Changes
	known := map[string]bool{}
	var missing []*queue.Source
	for _, src := range tracked {
		known[src.Path] = true
		f, ok := scan[src.Path]
		switch {
		case !ok:
			missing = append(missing, src)
		case f.Size != src.Size || f.MtimeNS != src.MtimeNS:
			c.Changed = append(c.Changed, src.Path)
		case src.MissingSince != 0:
			c.Returned = append(c.Returned, src.Path)
		}
	}

	added := map[File][]string{}
	for path, f := range scan {
		if !known[path] {
			added[f] = append(added[f], path)
		}
	}
	gone := map[File]int{}
	for _, src := range missing {
		gone[File{src.Size, src.MtimeNS}]++
	}
	for _, src := range missing {
		f := File{src.Size, src.MtimeNS}
		if paths := added[f]; len(paths) == 1 && gone[f] == 1 {
			c.Renamed = append(c.Renamed, Rename{From: src, To: paths[0]})
			delete(added, f)
			continue
		}
		c.Missing = append(c.Missing, src)
	}
	for _, paths := range added {
		c.New = append(c.New, paths...)
	}
	slices.Sort(c.New)
	return c
}
